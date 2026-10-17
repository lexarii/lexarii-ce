// SPDX-License-Identifier: AGPL-3.0-or-later

// Command lexarii is the Lexarii server: config, wiring, graceful shutdown.
// It holds no business logic of its own.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	stdhttp "net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"path/filepath"
	"scm.poniatowski.dev/adam/lexarii/internal/auth"
	"scm.poniatowski.dev/adam/lexarii/internal/config"
	"scm.poniatowski.dev/adam/lexarii/internal/domain"
	"scm.poniatowski.dev/adam/lexarii/internal/http"
	"scm.poniatowski.dev/adam/lexarii/internal/http/dto"
	"scm.poniatowski.dev/adam/lexarii/internal/ingest"
	"scm.poniatowski.dev/adam/lexarii/internal/llm"
	"scm.poniatowski.dev/adam/lexarii/internal/rag"
	"scm.poniatowski.dev/adam/lexarii/internal/store/postgres"
	"scm.poniatowski.dev/adam/lexarii/internal/worker"
)

// version is injected at build time via -ldflags.
var version = "dev"

func main() {
	// Deliberately before anything else, and answered without a database.
	//
	// This is what somebody running the container has to ask: an image tagged
	// `latest` says nothing about what is inside it, and the alternative -
	// starting the server and reading the first log line - needs a working
	// Postgres to get that far. It is also what makes a built image testable
	// in CI without standing up the rest of the installation.
	if len(os.Args) > 1 && (os.Args[1] == "-version" || os.Args[1] == "--version") {
		fmt.Println(version)
		return
	}

	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "lexarii: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("configuration: %w", err)
	}

	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level: parseLevel(cfg.LogLevel),
	})))

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Before the pool: an empty database is the normal state on a first run,
	// and every query after this depends on the schema existing. A container
	// that starts a server against an unmigrated database answers nothing and
	// says why in a hundred different places rather than one.
	if cfg.AutoMigrate {
		if err := postgres.Migrate(ctx, cfg.DatabaseURL, slog.Default()); err != nil {
			return fmt.Errorf("migrating the database: %w", err)
		}
	}

	pool, err := postgres.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return fmt.Errorf("database: %w", err)
	}
	defer pool.Close()
	slog.Info("database connected")

	// Both model clients are nil when unconfigured. That is a supported state:
	// the library and reader work without a model, and only enrichment and chat
	// degrade. Hence optional checks rather than required ones.
	chat := llm.New(cfg.LLM)
	embed := llm.New(cfg.Embed)

	// The width of the embedding column is fixed by the schema, and
	// LEXARII_EMBED_DIMENSIONS has to agree with it. It is checked here
	// rather than in config because only the database knows the answer.
	//
	// Refused rather than warned: a mismatch does not degrade, it fails every
	// insert, and it fails them one at a time inside a worker hours after the
	// process started successfully.
	if embed != nil {
		dims, err := pool.VectorDimensions(ctx)
		if err != nil {
			return fmt.Errorf("reading the embedding column width: %w", err)
		}
		if dims != cfg.Embed.Dimensions {
			return fmt.Errorf("LEXARII_EMBED_DIMENSIONS is %d but this database stores "+
				"%d-dimensional vectors; set it to %d, or re-embed the library against a "+
				"schema built for %d", cfg.Embed.Dimensions, dims, dims, cfg.Embed.Dimensions)
		}
	}

	optional := map[string]http.Checker{}
	if chat != nil {
		optional["llm"] = chat
	}
	if embed != nil {
		optional["embeddings"] = embed
	}

	// Authentication. Local passwords today; the registry is what lets OIDC,
	// LDAP, or a proxy provider be added as configuration rather than as a
	// rewrite.
	// Local passwords are always available, even when a federated provider is
	// configured: an issuer that is unreachable or misconfigured must not lock
	// the owner out of their own library.
	// The first administrator, when one is named in the environment.
	//
	// A fresh install offers a setup form to whoever reaches it first. That is
	// right for somebody sitting at the machine and wrong for a container
	// started on a network, where "whoever reaches it first" is a race with
	// the internet. Naming the account in the environment means it exists
	// before the port opens.
	//
	// Only when the installation has NO users. It is a bootstrap, not a
	// password reset: re-running with the variables still set must not silently
	// put the password back to whatever is in a compose file, and must not
	// undo a change made since. `make password` is the reset.
	if cfg.Admin.Wanted() {
		created, err := bootstrapAdmin(ctx, pool, cfg.Admin)
		if err != nil {
			return fmt.Errorf("creating the first administrator: %w", err)
		}
		if created {
			slog.Info("created the first administrator from the environment",
				"username", cfg.Admin.Username)
		}
	}

	configured := []auth.Provider{&auth.LocalProvider{Store: pool}}

	if cfg.OIDC.Enabled() {
		oidc, err := auth.NewOIDCProvider(auth.OIDCConfig{
			Issuer:       cfg.OIDC.Issuer,
			ClientID:     cfg.OIDC.ClientID,
			ClientSecret: cfg.OIDC.ClientSecret,
			Label:        cfg.OIDC.Label,
			Scopes:       cfg.OIDC.Scopes,
			AllowSignup:  cfg.OIDC.AllowSignup,
		})
		if err != nil {
			return fmt.Errorf("configuring OIDC: %w", err)
		}
		configured = append(configured, oidc)
		slog.Info("OIDC sign-in enabled", "issuer", cfg.OIDC.Issuer, "allowSignup", cfg.OIDC.AllowSignup)
	}

	if cfg.LDAP.Enabled() {
		ldapCfg := auth.LDAPConfig{
			URL:                cfg.LDAP.URL,
			StartTLS:           cfg.LDAP.StartTLS,
			InsecureSkipVerify: cfg.LDAP.InsecureSkipVerify,
			BindDN:             cfg.LDAP.BindDN,
			BindPassword:       cfg.LDAP.BindPassword,
			BaseDN:             cfg.LDAP.BaseDN,
			UserFilter:         cfg.LDAP.UserFilter,
			UserDNTemplate:     cfg.LDAP.UserDNTemplate,
			Label:              cfg.LDAP.Label,
			RequiredGroup:      cfg.LDAP.RequiredGroup,
			AllowSignup:        cfg.LDAP.AllowSignup,
		}
		if cfg.LDAP.ActiveDirectory {
			ldapCfg = auth.ActiveDirectoryDefaults(ldapCfg)
		}

		directory, err := auth.NewLDAPProvider(ldapCfg)
		if err != nil {
			return fmt.Errorf("configuring the directory: %w", err)
		}
		configured = append(configured, directory)

		if cfg.LDAP.InsecureSkipVerify {
			slog.Warn("directory certificate verification is disabled; credentials can be intercepted")
		}
		if !cfg.LDAP.StartTLS && !strings.HasPrefix(cfg.LDAP.URL, "ldaps://") {
			slog.Warn("directory connection is not encrypted; passwords cross the network in the clear",
				"url", cfg.LDAP.URL)
		}
		slog.Info("directory sign-in enabled",
			"url", cfg.LDAP.URL, "activeDirectory", cfg.LDAP.ActiveDirectory,
			"requiredGroup", cfg.LDAP.RequiredGroup, "allowSignup", cfg.LDAP.AllowSignup)
	}

	var proxyProvider *auth.ProxyProvider
	if cfg.Proxy.Enabled() {
		proxyProvider, err = auth.NewProxyProvider(auth.ProxyConfig{
			UserHeader:     cfg.Proxy.UserHeader,
			EmailHeader:    cfg.Proxy.EmailHeader,
			NameHeader:     cfg.Proxy.NameHeader,
			GroupsHeader:   cfg.Proxy.GroupsHeader,
			TrustedProxies: cfg.Proxy.TrustedProxies,
			RequiredGroup:  cfg.Proxy.RequiredGroup,
			AllowSignup:    cfg.Proxy.AllowSignup,
			Label:          cfg.Proxy.Label,
		})
		if err != nil {
			return fmt.Errorf("configuring proxy authentication: %w", err)
		}
		configured = append(configured, proxyProvider)
		slog.Info("proxy authentication enabled",
			"header", cfg.Proxy.UserHeader, "trusted", cfg.Proxy.TrustedProxies)
	}

	// This installation's own reverse proxies, whether or not one of them also
	// asserts identities.
	//
	// Behind nginx or Caddy every request arrives from 127.0.0.1, so a sign-in
	// throttle keyed on the socket address would put the whole household in one
	// bucket and let eight wrong passwords lock everybody out. Naming the proxy
	// here is what lets a failed attempt be counted against the reader who made
	// it. Unset is correct when nothing sits in front of this.
	trustedProxies, err := auth.ParseTrustedProxies(cfg.Proxy.TrustedProxies)
	if err != nil {
		return fmt.Errorf("LEXARII_PROXY_TRUSTED: %w", err)
	}
	if !trustedProxies.Empty() {
		slog.Info("trusting X-Forwarded-For from these proxies for rate limiting",
			"trusted", cfg.Proxy.TrustedProxies)
	}

	providers := auth.NewRegistry(configured...)
	sessions := &auth.Sessions{Store: pool, Secure: cfg.SecureCookies}

	if !cfg.SecureCookies {
		slog.Warn("session cookies are not marked Secure; use this only on a trusted network over plain HTTP")
	}

	hasUser, err := pool.HasAnyUser(ctx)
	if err != nil {
		return fmt.Errorf("checking for accounts: %w", err)
	}
	if !hasUser {
		slog.Info("no accounts yet; open the web interface to create the first one")
	}

	// Covers live under the DATA directory. A derived artifact never goes next
	// to somebody's books, and the library directory is read-only.
	coverDir := filepath.Join(cfg.DataDir, "covers")
	// Where the books of departed holders are kept. Shared by the account
	// handler, which moves files in, and the ingester, which removes a copy a
	// fresh upload has replaced.
	archiveDir := cfg.ArchiveDir()

	ingester := &worker.Ingest{
		Store:      pool,
		LibraryDir: cfg.LibraryDir,
		CoverDir:   coverDir,
		ArchiveDir: archiveDir,
		UploadDir:  cfg.UploadDir,
		Logger:     slog.Default(),
	}
	if cfg.IndexOnIngest {
		ingester.Indexing = pool
	}
	// The LLM is optional: with no chat model, enrichment on ingest stays off
	// rather than filling the queue with jobs that can only wait.
	switch {
	case cfg.EnrichOnIngest && cfg.LLM.Enabled():
		ingester.Enriching = pool
	case cfg.EnrichOnIngest:
		slog.Warn("LEXARII_ENRICH_ON_INGEST is set but no chat model is configured; new books are not enriched")
	}
	// Enrichment is optional by invariant: with no model configured the handler
	// is still registered, and every enrichment job politely waits instead of
	// failing. That is what lets someone queue a library today and switch the
	// GPU machine on tomorrow.
	enricher := &worker.Enrich{
		Store:       pool,
		Client:      chat,
		Logger:      slog.Default(),
		SampleChars: cfg.EnrichSampleChars,
	}

	// Chunking needs no model at all, and embedding needs only the embedding
	// one, so a library can be made searchable in stages as machines come and
	// go.
	indexer := &worker.Index{
		Store:      pool,
		Client:     embed,
		Logger:     slog.Default(),
		Dimensions: cfg.Embed.Dimensions,
	}

	handlers := ingester.Handlers()
	for _, set := range []map[domain.JobType]worker.Handler{
		enricher.Handlers(),
		indexer.Handlers(),
	} {
		for jobType, handler := range set {
			handlers[jobType] = handler
		}
	}

	workers := worker.New(pool, handlers, worker.Config{
		Workers: cfg.WorkerCount,
		Logger:  slog.Default(),
		// Books left behind by a deleted account wait to be claimed, and their
		// RECORDS are deleted once nobody has. The files are kept: a catalogue
		// entry naming somebody who left should not be held forever, and a book
		// on a disk costs nothing to leave alone.
		Archive:          pool,
		ArchiveRetention: cfg.ArchiveRetention,
		// Erasing takes an expired record's stored copies with it.
		EraseExpired: cfg.DeletedAccountFiles.Erase(),
		// Author rows are tidied as books arrive: a name that is not a person
		// is detached, and two spellings of one person become one. Both rules
		// are deterministic and neither asks a model, which is what makes doing
		// it unattended defensible - see postgres.NormaliseAuthors.
		Authors: pool,
		// Finished jobs are deleted after LEXARII_JOB_RETENTION: an upload's
		// names the file somebody uploaded, and nothing runs it again.
		Jobs:         pool,
		JobRetention: cfg.JobRetention,
	})

	// Dead sessions are housekeeping, not enforcement: an expired one is already
	// refused at validation. This stops the table growing without bound.
	go auth.SweepSessions(ctx, pool, auth.SweepInterval, auth.SweepRetention, slog.Default())

	workerDone := make(chan struct{})
	go func() {
		defer close(workerDone)
		if err := workers.Run(ctx); err != nil {
			slog.Error("worker pool stopped", "error", err)
		}
	}()

	// A reranker is a third model endpoint, not a flag: it is a cross-encoder,
	// which is a different kind of model from the one that writes answers.
	// Configuring one turns the stage on; there is nothing to turn on without
	// one.
	reranker := llm.New(cfg.Rerank)
	if reranker != nil {
		optional["reranker"] = reranker
		slog.Info("reranking enabled", "model", cfg.Rerank.Model, "endpoint", cfg.Rerank.BaseURL)
	}

	answerer := &rag.Answerer{
		Store:         pool,
		Chat:          chat,
		Embed:         embed,
		Dimensions:    cfg.Embed.Dimensions,
		Reranker:      reranker,
		ContextTokens: cfg.LLM.ContextTokens,
	}

	// Removing a file for everyone deletes the copies Lexarii stored, and
	// only those: uploads, the archive, covers - never anything inside the
	// library directory, which the blocklist keeps off every shelf instead.
	moderation := &http.ModerationHandler{
		Store:    pool,
		Accounts: pool,
		Discard: ingest.Discard{Uploads: cfg.UploadDir, Archive: archiveDir, Covers: coverDir,
			Library: cfg.LibraryDir},
	}
	// Copies a removal could not delete - the server stopped first, or the
	// disk refused - are finished here, once, in the background.
	go moderation.FinishRemovals(ctx)

	accounts := &http.AccountHandler{
		Store: pool,
		// The archive lives in Lexarii's own data directory, and only
		// moves files Lexarii itself stored. A book scanned from the
		// library directory is left where it is: that directory is
		// read-only to us, which is the first invariant in this project.
		Archive: ingest.Archive{
			Root:    archiveDir,
			Managed: cfg.UploadDir,
			Library: cfg.LibraryDir,
		},
		// Erasing deletes what Lexarii stored, exactly as removing a
		// file for everyone does, and never anything in the library
		// directory.
		Eraser: pool,
		Discard: ingest.Discard{Uploads: cfg.UploadDir, Archive: archiveDir, Covers: coverDir,
			Library: cfg.LibraryDir},
		EraseOnDelete: cfg.DeletedAccountFiles.Erase(),
	}
	// What erasures still owe the disk - copies the disk refused, a server
	// stopped between an erasure and its clean-up, an archive record expired
	// in erase mode - is paid now and every hour, in the background.
	go accounts.FinishErasures(ctx, time.Hour)

	// The installation's own look for the sign-in page, when it has one.
	// Config has already checked the directory exists.
	var branding stdhttp.Handler
	if cfg.BrandingDir != "" {
		files, err := http.NewBrandingFiles(cfg.BrandingDir, slog.Default())
		if err != nil {
			return fmt.Errorf("LEXARII_BRANDING_DIR: %w", err)
		}
		branding = files
		// The client loads branding.css and nothing else by name, so a
		// directory without one changes nothing; say so rather than leave
		// somebody wondering why.
		if _, err := os.Stat(filepath.Join(cfg.BrandingDir, "branding.css")); err != nil {
			slog.Warn("LEXARII_BRANDING_DIR has no branding.css; the client loads that file and nothing else by name",
				"dir", cfg.BrandingDir)
		}
	}

	router := &http.Router{
		Branding: branding,
		Health: &http.HealthHandler{
			Version:  version,
			Required: map[string]http.Checker{"postgres": pool},
			Optional: optional,
			Accounts: pool,
		},
		Sessions: sessions,
		Proxy:    proxyProvider,
		Auth: &http.AuthHandler{
			Store:     pool,
			Providers: providers,
			Sessions:  sessions,
			Throttle:  auth.NewThrottle(),
			Trusted:   trustedProxies,
			Log:       slog.Default(),

			AnyoneMayUpload: cfg.UploadPolicy.AnyoneMayUpload(),
			ChatConfigured:  cfg.LLM.Enabled(),
			PeopleUnlisted:  !cfg.PeopleDirectory.Listed(),
			// Validated by config at startup.
			SignInLinks: signInLinks(cfg.SignInLinks),
			Branding:    branding != nil,
		},
		Library:     &http.LibraryHandler{Store: pool, Accounts: pool},
		Jobs:        &http.JobHandler{Queue: pool, Accounts: pool},
		Reader:      &http.ReaderHandler{Store: pool, PDFCacheBytes: cfg.PDFCacheBytes},
		Progress:    &http.ProgressHandler{Store: pool},
		Sync:        &http.SyncHandler{Store: pool},
		OPDS:        &http.OPDSHandler{Store: pool},
		Annotations: &http.AnnotationHandler{Store: pool},
		Ask:         &http.AskHandler{Answerer: answerer},
		Chat: &http.ChatHandler{
			Store:    pool,
			Answerer: answerer,
		},
		Enrichment: &http.EnrichmentHandler{
			Store: pool,
			Queue: pool,
			// Reported to the client so it can say "switch your model on"
			// rather than leaving someone wondering why nothing happened.
			ModelConfigured: cfg.LLM.Enabled(),
			EmbedModel:      embed.Model(),
			Accounts:        pool,
		},
		Sharing: &http.SharingHandler{
			Store:    pool,
			Unlisted: !cfg.PeopleDirectory.Listed(),
			Accounts: pool,
			// Names that match nobody, counted per person: lending by name
			// cannot hide whether one exists, so it is not a free way to try
			// a list of them.
			UnknownNames: auth.NewThrottle(),
		},
		Accounts:    accounts,
		Covers:      &http.CoverHandler{Store: pool, Dir: coverDir},
		Citations:   &http.CitationHandler{Store: pool, BaseURL: cfg.PublicURL},
		Collections: &http.CollectionHandler{Store: pool},
		Moderation:  moderation,
		Uploads: &http.UploadHandler{
			Store:    &ingest.UploadStore{Root: cfg.UploadDir, MaxFileSize: cfg.MaxUploadBytes},
			Queue:    pool,
			Log:      slog.Default(),
			Accounts: pool,

			AnyoneMayUpload: cfg.UploadPolicy.AnyoneMayUpload(),
		},
	}

	srv := &stdhttp.Server{
		Addr:              cfg.Addr,
		Handler:           router.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		slog.Info("listening", "addr", cfg.Addr, "version", version,
			"libraryDir", cfg.LibraryDir, "dataDir", cfg.DataDir, "uploadDir", cfg.UploadDir,
			"llmConfigured", cfg.LLM.Enabled(), "embedConfigured", cfg.Embed.Enabled())
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, stdhttp.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		return fmt.Errorf("serving: %w", err)
	case <-ctx.Done():
		slog.Info("shutting down")
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("shutdown: %w", err)
	}

	// Workers are already winding down: they share the signal context. A job
	// still running when the process exits is left marked running and returned
	// to the queue by the reclaimer, which is safe because handlers are
	// idempotent.
	select {
	case <-workerDone:
		slog.Info("workers stopped")
	case <-shutdownCtx.Done():
		slog.Warn("workers did not stop in time; in-flight jobs will be reclaimed")
	}
	return nil
}

func parseLevel(s string) slog.Level {
	var l slog.Level
	if err := l.UnmarshalText([]byte(s)); err != nil {
		return slog.LevelInfo
	}
	return l
}

// bootstrapAdmin creates the account named in the environment, once.
//
// Reports whether it made one. The password is never logged, and the check is
// "has this installation any users at all" rather than "does this username
// exist": an installation with somebody else's account already in it is not a
// fresh one, and quietly adding an administrator to it would be a way in that
// its owner never asked for.
func bootstrapAdmin(ctx context.Context, pool *postgres.Pool, admin config.AdminAccount) (bool, error) {
	has, err := pool.HasAnyUser(ctx)
	if err != nil {
		return false, err
	}
	if has {
		return false, nil
	}

	if _, err := pool.AdoptOrCreateLocalUser(ctx, admin.Username, admin.Email, admin.Password, true); err != nil {
		return false, err
	}
	return true, nil
}

// signInLinks carries the configured sign-in page links onto the wire type.
func signInLinks(links []config.SignInLink) []dto.SignInLink {
	out := make([]dto.SignInLink, 0, len(links))
	for _, l := range links {
		out = append(out, dto.SignInLink{Label: l.Label, Href: l.Href})
	}
	return out
}
