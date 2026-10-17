// SPDX-License-Identifier: AGPL-3.0-or-later

// Package config loads and validates Lexarii's configuration from the
// environment. Validation happens once at startup and the process exits with a
// message naming the offending variable - never boot half-configured.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"scm.poniatowski.dev/adam/lexarii/internal/domain"
)

// Config is the fully validated application configuration.
type Config struct {
	Addr     string
	LogLevel string
	// PublicURL is how this installation is reached from outside, e.g.
	// https://books.example. Used where a link has to survive leaving the
	// application - the url field of an exported citation, so far.
	//
	// Empty by default and left out rather than guessed. A bibliography
	// carrying http://localhost:8080 is worse than one carrying no link: the
	// citation goes into somebody else's document, where that address means
	// their own machine.
	PublicURL string
	// MetadataBaseURL is an OpenLibrary-compatible catalogue, for filling in
	// publishers, years and ISBNs that a file did not carry.
	//
	// Empty by default, and that is the point: asking a catalogue means
	// sending a description of somebody's books to a third party. "No
	// telemetry, no phoning home" is a promise this would otherwise break
	// quietly. https://openlibrary.org turns it on; a self-hosted copy works
	// too, which is why it is a URL rather than a boolean.
	MetadataBaseURL   string
	MetadataUserAgent string
	MetadataTimeout   time.Duration
	// PDFCacheBytes is how much memory the reader's parsed PDFs may hold.
	//
	// Bytes rather than a book count, because books are not comparable: one
	// in a real library parses to 76MB and another to 176MB, so "four books"
	// means either 300MB or 700MB depending which four. Set from
	// LEXARII_PDF_CACHE_MB.
	PDFCacheBytes int64
	LibraryDir    string // read-only: Lexarii never writes here
	DataDir       string // derived artifacts: covers, extracted text, thumbnails
	// UploadDir is Lexarii's OWN managed storage for uploaded books. It is
	// deliberately distinct from LibraryDir: uploads have to be written
	// somewhere, and writing them into the user's library directory would break
	// the read-only promise that makes people willing to point this at a
	// twenty-year collection. Defaults to <DataDir>/uploads.
	UploadDir string
	// MaxUploadBytes caps a single uploaded file.
	MaxUploadBytes int64
	// UploadPolicy says who may add books by uploading them:
	// LEXARII_UPLOAD_POLICY, UploadAdmins by default.
	UploadPolicy UploadPolicy
	// PeopleDirectory says whether every signed-in account may list every
	// other one: LEXARII_PEOPLE_DIRECTORY, DirectoryAll by default.
	PeopleDirectory PeopleDirectory
	// DeletedAccountFiles says what happens to the books only a deleted
	// account held: LEXARII_DELETED_ACCOUNT_FILES, FilesArchive by default.
	DeletedAccountFiles DeletedAccountFiles

	// ArchiveRetention is how long the books of a deleted account wait to be
	// claimed before their catalogue records are deleted. The FILES are kept
	// whatever this says: the retention is about how long a record naming
	// somebody who left should be held, not about throwing books away.
	//
	// Six months by default, which is a guess at "long enough that somebody who
	// left and came back finds their library". An installation with a data
	// protection policy of its own should set this to match it.
	ArchiveRetention time.Duration

	// JobRetention is how long a finished job is kept before it is deleted:
	// LEXARII_JOB_RETENTION, domain.DefaultJobRetention (30 days) by default,
	// and never less than domain.MinJobRetention (7 days), which enrichment
	// on ingest reads back. An upload's job names the file uploaded; the
	// queue is not meant to be a permanent record of that.
	JobRetention time.Duration

	// AutoMigrate applies pending migrations at startup.
	//
	// On by default, because the install this promises is one binary and a
	// DATABASE_URL: a second command that must be run first, and re-run after
	// every upgrade, is a step somebody will miss at exactly the moment they
	// are least equipped to debug it. An operator who manages schema out of
	// band sets LEXARII_AUTO_MIGRATE=false and runs `make migrate-up`.
	AutoMigrate bool

	// Rerank is the cross-encoder that reorders retrieved passages.
	//
	// Its own endpoint, because a cross-encoder is a different kind of model
	// from the one that writes answers - it reads a question and one passage
	// together and scores the pair. Unset means no reranking, which is the
	// default and was measured: asking the CHAT model to do this job cost 0.081
	// of work Recall@10 and 0.167 of multi-book coverage and took the median
	// question from 1.6 to 6.8 seconds.
	//
	// llama.cpp serves one with `--reranking --embedding --pooling rank`.
	Rerank ModelConfig

	DatabaseURL string

	LLM   ModelConfig
	Embed ModelConfig

	WorkerCount int

	// IndexOnIngest queues chunking for every file as it is ingested, by
	// upload or by scan, so a new book is searchable without an administrator
	// pressing "index". Embedding follows chunking as it always does, when an
	// embedding model is configured.
	//
	// Off by default, which keeps indexing a deliberate step: chunking is cheap
	// but the embedding behind it is GPU time, and a first scan of a large
	// library is a lot of it. Set from LEXARII_INDEX_ON_INGEST=true|false.
	IndexOnIngest bool

	// EnrichOnIngest queues enrichment for every book as it is ingested, by
	// upload or by scan, so a new book is described by the model without an
	// administrator pressing "describe". The job is for whoever added the
	// book, who may follow it.
	//
	// Off by default for the same reason as IndexOnIngest: it is model time,
	// and a first scan of a large library is a lot of it. Ignored with no
	// chat model configured - every job would only wait for one. Set from
	// LEXARII_ENRICH_ON_INGEST=true|false.
	EnrichOnIngest bool

	// SignInLinks are the administrator's own links for the sign-in page:
	// how to ask for an account, where to reset a password, the terms of the
	// place. Lexarii has no opinion on any of those, so it shows what it is
	// given. From LEXARII_SIGNIN_LINKS; see ParseSignInLinks.
	SignInLinks []SignInLink

	// BrandingDir is a directory of the installation's own static files - a
	// branding.css and the images and fonts it uses - served read-only at
	// /branding/ and loaded by the client after its own styles, so the
	// sign-in page can carry the installation's look without a fork. Empty,
	// the default, serves nothing. From LEXARII_BRANDING_DIR; must exist and
	// be a directory. See docs/BRANDING.md.
	BrandingDir string

	// EnrichSampleChars is how much of a book one enrichment sends to the
	// model. Zero uses the enricher's own default.
	//
	// The single dial for "enrichment is too slow": the cost of a book is
	// almost entirely the model reading this sample - extracting it takes
	// hundredths of a second - so halving it roughly halves the time per book,
	// at the cost of giving the model less to go on.
	EnrichSampleChars int

	// SecureCookies marks session cookies Secure, so a browser will only send
	// them over HTTPS. On by default: turning it off is only correct for plain
	// HTTP on a trusted network, and the server logs a warning when it is.
	SecureCookies bool

	// Admin is an account created at startup when the installation has none.
	//
	// A fresh install offers a setup form to whoever reaches it first, which
	// is right for somebody sitting at the machine and wrong for a container
	// started on a network. This is the other way in: name the first
	// administrator in the environment and it exists before the port opens.
	Admin AdminAccount

	// OIDC is configured when an issuer is set. Local passwords keep working
	// alongside it, so a misconfigured issuer cannot lock everyone out.
	OIDC OIDCConfig

	// LDAP covers both LDAP and Active Directory; the flavour switches the
	// attribute defaults, which differ in every name that matters.
	LDAP LDAPConfig

	// Proxy trusts an authenticating reverse proxy. Requires an explicit list of
	// trusted addresses: without one, anyone who can reach the port asserts any
	// identity they like.
	Proxy ProxyAuthConfig
}

// ProxyAuthConfig configures trust in a reverse proxy.
type ProxyAuthConfig struct {
	UserHeader     string
	EmailHeader    string
	NameHeader     string
	GroupsHeader   string
	TrustedProxies []string
	RequiredGroup  string
	AllowSignup    bool
	Label          string
}

// Enabled reports whether proxy authentication is configured.
func (p ProxyAuthConfig) Enabled() bool { return p.UserHeader != "" }

// LDAPConfig configures directory sign-in.
type LDAPConfig struct {
	URL      string
	StartTLS bool
	// InsecureSkipVerify disables certificate checks. Internal directories
	// routinely use a private CA; the server logs a warning when this is on.
	InsecureSkipVerify bool

	BindDN         string
	BindPassword   string
	BaseDN         string
	UserFilter     string
	UserDNTemplate string

	// ActiveDirectory switches the attribute defaults to AD's names.
	ActiveDirectory bool

	Label         string
	RequiredGroup string
	AllowSignup   bool
}

// Enabled reports whether directory sign-in is configured.
func (l LDAPConfig) Enabled() bool { return l.URL != "" }

// OIDCConfig configures OpenID Connect sign-in.
type OIDCConfig struct {
	Issuer       string
	ClientID     string
	ClientSecret string
	Label        string
	Scopes       []string
	// AllowSignup permits creating an account on first sign-in. Off by default:
	// against a public issuer it would let anyone with an account there in.
	AllowSignup bool
}

// Enabled reports whether OIDC is configured.
func (o OIDCConfig) Enabled() bool { return o.Issuer != "" && o.ClientID != "" }

// ModelConfig points at an OpenAI-compatible endpoint. The chat model and the
// embedding model are configured separately because they are frequently
// different servers on different machines.
type ModelConfig struct {
	BaseURL        string
	Model          string
	APIKey         string
	ContextTokens  int
	Dimensions     int // embeddings only
	MaxConcurrency int
	Timeout        time.Duration

	// ExtraBody is merged into every chat request body, which is how a
	// provider's own request fields (OpenRouter's "provider" routing
	// preferences, for one) reach it without a code path per provider.
	// Chat model only; see ParseExtraBody.
	ExtraBody map[string]json.RawMessage

	// ExtraHeaders are sent on every request to the endpoint, such as
	// OpenRouter's optional HTTP-Referer and X-Title. Authorization is refused;
	// see ParseExtraHeaders. Treat the values as secrets: never log them.
	ExtraHeaders map[string]string

	// ReasoningEffort overrides what callers ask for. Empty leaves their choice
	// alone, ReasoningDefault stops the field being sent at all, and anything
	// else is sent in its place. Chat model only.
	ReasoningEffort string
}

// Enabled reports whether this model is configured at all. Lexarii must stay
// a complete library and reader with no model configured or reachable.
func (m ModelConfig) Enabled() bool { return m.BaseURL != "" }

// UploadPolicy is who may add books by uploading them.
type UploadPolicy string

const (
	// UploadAdmins lets only administrators upload. The default, and what
	// every installation did before the setting existed.
	UploadAdmins UploadPolicy = "admins"
	// UploadUsers lets anybody signed in upload into their own shelf. A
	// household or a club where each person brings their own books wants
	// this; the books stay theirs until they lend them.
	UploadUsers UploadPolicy = "users"
)

func (p UploadPolicy) valid() bool {
	return p == UploadAdmins || p == UploadUsers
}

// AnyoneMayUpload reports whether every signed-in account may upload.
func (p UploadPolicy) AnyoneMayUpload() bool { return p == UploadUsers }

// PeopleDirectory is who may list the installation's accounts, which is what
// a share picker offers to lend a book to.
type PeopleDirectory string

const (
	// DirectoryAll lists every account's username to anybody signed in. The
	// default, and what every installation did before the setting existed:
	// in a household, being able to pick a name from a list is the point.
	DirectoryAll PeopleDirectory = "all"
	// DirectoryNone lists accounts to administrators only. On a large
	// installation the list is every member of it, handed to each of them;
	// with this, somebody lending a book names the recipient by their exact
	// username instead.
	DirectoryNone PeopleDirectory = "none"
)

func (d PeopleDirectory) valid() bool {
	return d == DirectoryAll || d == DirectoryNone
}

// Listed reports whether a non-administrator may list every account.
func (d PeopleDirectory) Listed() bool { return d != DirectoryNone }

// DeletedAccountFiles is what happens to the books only a deleted account
// held. Books somebody else also holds simply lose a holder either way.
type DeletedAccountFiles string

const (
	// FilesArchive keeps them, archived against the leaver's name, for an
	// administrator to hand on. The default, and what every installation did
	// before the setting existed.
	FilesArchive DeletedAccountFiles = "archive"
	// FilesErase erases them: their records, what was derived from them, and
	// the copies Lexarii stored - never anything in the library directory.
	// For an installation that must not keep what a person added once they
	// are gone.
	FilesErase DeletedAccountFiles = "erase"
)

func (f DeletedAccountFiles) valid() bool {
	return f == FilesArchive || f == FilesErase
}

// Erase reports whether a deleted account's books are erased.
func (f DeletedAccountFiles) Erase() bool { return f == FilesErase }

// Load reads configuration from the environment and validates it.
func Load() (Config, error) {
	var errs []error
	fail := func(format string, a ...any) { errs = append(errs, fmt.Errorf(format, a...)) }

	cfg := Config{
		Addr:              env("LEXARII_ADDR", ":8080"),
		LogLevel:          env("LEXARII_LOG_LEVEL", "info"),
		LibraryDir:        env("LEXARII_LIBRARY_DIR", "./library"),
		DataDir:           env("LEXARII_DATA_DIR", "./data"),
		ArchiveRetention:  envDuration("LEXARII_ARCHIVE_RETENTION", domain.DefaultArchiveRetention, fail),
		JobRetention:      envDuration("LEXARII_JOB_RETENTION", domain.DefaultJobRetention, fail),
		UploadDir:         env("LEXARII_UPLOAD_DIR", ""),
		DatabaseURL:       env("DATABASE_URL", ""),
		WorkerCount:       envInt("LEXARII_WORKER_COUNT", 4, fail),
		IndexOnIngest:     envBool("LEXARII_INDEX_ON_INGEST", false, fail),
		EnrichOnIngest:    envBool("LEXARII_ENRICH_ON_INGEST", false, fail),
		EnrichSampleChars: envInt("LEXARII_ENRICH_SAMPLE_CHARS", 0, fail),
		// Default true. A self-hoster on plain HTTP must opt out deliberately
		// rather than discover the cookie was never protected.
		SecureCookies: env("LEXARII_SECURE_COOKIES", "true") != "false",
		Admin: AdminAccount{
			Username: strings.TrimSpace(env("LEXARII_ADMIN_USERNAME", "")),
			Password: env("LEXARII_ADMIN_PASSWORD", ""),
			Email:    strings.TrimSpace(env("LEXARII_ADMIN_EMAIL", "")),
		},
		OIDC: OIDCConfig{
			Issuer:       env("LEXARII_OIDC_ISSUER", ""),
			ClientID:     env("LEXARII_OIDC_CLIENT_ID", ""),
			ClientSecret: env("LEXARII_OIDC_CLIENT_SECRET", ""),
			Label:        env("LEXARII_OIDC_LABEL", "Single sign-on"),
			Scopes:       splitList(env("LEXARII_OIDC_SCOPES", "")),
			AllowSignup:  env("LEXARII_OIDC_ALLOW_SIGNUP", "false") == "true",
		},
		LDAP: LDAPConfig{
			URL: env("LEXARII_LDAP_URL", ""),
			// Default TRUE, opt out, like SecureCookies. The example file has
			// always said true while this said false, so anybody who wrote
			// their own env rather than copying the example got a plain
			// connection - and what crosses a plain LDAP connection is
			// somebody's password. Skipped anyway for ldaps://.
			StartTLS:           env("LEXARII_LDAP_STARTTLS", "true") != "false",
			InsecureSkipVerify: env("LEXARII_LDAP_INSECURE_SKIP_VERIFY", "false") == "true",
			BindDN:             env("LEXARII_LDAP_BIND_DN", ""),
			BindPassword:       env("LEXARII_LDAP_BIND_PASSWORD", ""),
			BaseDN:             env("LEXARII_LDAP_BASE_DN", ""),
			UserFilter:         env("LEXARII_LDAP_USER_FILTER", ""),
			UserDNTemplate:     env("LEXARII_LDAP_USER_DN_TEMPLATE", ""),
			ActiveDirectory:    env("LEXARII_LDAP_ACTIVE_DIRECTORY", "false") == "true",
			Label:              env("LEXARII_LDAP_LABEL", ""),
			RequiredGroup:      env("LEXARII_LDAP_REQUIRED_GROUP", ""),
			AllowSignup:        env("LEXARII_LDAP_ALLOW_SIGNUP", "false") == "true",
		},
		Proxy: ProxyAuthConfig{
			// The user header is the switch: blank disables the provider.
			UserHeader: env("LEXARII_PROXY_USER_HEADER", ""),
			// The other three default to what oauth2-proxy, Authelia and
			// Authentik all send, so a working setup is one variable rather
			// than four. They only do anything once UserHeader names a header,
			// and a proxy that sends different names sets them.
			EmailHeader:    env("LEXARII_PROXY_EMAIL_HEADER", "X-Forwarded-Email"),
			NameHeader:     env("LEXARII_PROXY_NAME_HEADER", "X-Forwarded-Preferred-Username"),
			GroupsHeader:   env("LEXARII_PROXY_GROUPS_HEADER", "X-Forwarded-Groups"),
			TrustedProxies: splitList(env("LEXARII_PROXY_TRUSTED", "")),
			RequiredGroup:  env("LEXARII_PROXY_REQUIRED_GROUP", ""),
			AllowSignup:    env("LEXARII_PROXY_ALLOW_SIGNUP", "true") != "false",
			Label:          env("LEXARII_PROXY_LABEL", ""),
		},
		LLM: ModelConfig{
			BaseURL:        env("LEXARII_LLM_BASE_URL", ""),
			Model:          env("LEXARII_LLM_MODEL", ""),
			APIKey:         env("LEXARII_LLM_API_KEY", ""),
			ContextTokens:  envInt("LEXARII_LLM_CONTEXT_TOKENS", 8192, fail),
			MaxConcurrency: envInt("LEXARII_LLM_MAX_CONCURRENCY", 2, fail),
			Timeout:        envDuration("LEXARII_LLM_TIMEOUT", 120*time.Second, fail),
		},
		AutoMigrate: env("LEXARII_AUTO_MIGRATE", "true") != "false",
		PublicURL:   strings.TrimRight(env("LEXARII_PUBLIC_URL", ""), "/"),

		MetadataBaseURL:   strings.TrimRight(env("LEXARII_METADATA_BASE_URL", ""), "/"),
		MetadataUserAgent: env("LEXARII_METADATA_USER_AGENT", ""),
		MetadataTimeout:   envDuration("LEXARII_METADATA_TIMEOUT", 10*time.Second, fail),
		PDFCacheBytes:     int64(envInt("LEXARII_PDF_CACHE_MB", 0, fail)) << 20,

		Rerank: ModelConfig{
			BaseURL: strings.TrimRight(env("LEXARII_RERANK_BASE_URL", ""), "/"),
			Model:   env("LEXARII_RERANK_MODEL", ""),
			APIKey:  env("LEXARII_RERANK_API_KEY", ""),
			Timeout: envDuration("LEXARII_RERANK_TIMEOUT", 30*time.Second, fail),
			// One at a time. Reranking happens while somebody waits, and a
			// second concurrent request would queue inside the server anyway.
			MaxConcurrency: 1,
		},

		Embed: ModelConfig{
			BaseURL:        env("LEXARII_EMBED_BASE_URL", ""),
			Model:          env("LEXARII_EMBED_MODEL", ""),
			APIKey:         env("LEXARII_EMBED_API_KEY", ""),
			Dimensions:     envInt("LEXARII_EMBED_DIMENSIONS", 768, fail),
			MaxConcurrency: envInt("LEXARII_EMBED_MAX_CONCURRENCY", 4, fail),
			Timeout:        envDuration("LEXARII_EMBED_TIMEOUT", 60*time.Second, fail),
		},
	}

	var parseErr error
	if cfg.LLM.ExtraBody, parseErr = ParseExtraBody(os.Getenv("LEXARII_LLM_EXTRA_BODY")); parseErr != nil {
		fail("LEXARII_LLM_EXTRA_BODY %v", parseErr)
	}
	if cfg.LLM.ExtraHeaders, parseErr = ParseExtraHeaders(os.Getenv("LEXARII_LLM_EXTRA_HEADERS")); parseErr != nil {
		fail("LEXARII_LLM_EXTRA_HEADERS %v", parseErr)
	}
	cfg.LLM.ReasoningEffort = strings.ToLower(env("LEXARII_LLM_REASONING_EFFORT", ""))
	if cfg.LLM.ReasoningEffort != "" && cfg.LLM.ReasoningEffort != ReasoningDefault && hasKeyFold(cfg.LLM.ExtraBody, "reasoning") {
		fail("LEXARII_LLM_REASONING_EFFORT and a \"reasoning\" field in LEXARII_LLM_EXTRA_BODY both set the " +
			"reasoning effort; use one (with \"reasoning\" in the body, leave the effort blank)")
	}
	if strings.ContainsAny(cfg.LLM.ReasoningEffort, " \t\"") {
		fail("LEXARII_LLM_REASONING_EFFORT must be one word such as none, low, high or %s", ReasoningDefault)
	}

	if cfg.SignInLinks, parseErr = ParseSignInLinks(os.Getenv("LEXARII_SIGNIN_LINKS")); parseErr != nil {
		fail("LEXARII_SIGNIN_LINKS %v", parseErr)
	}

	cfg.BrandingDir = env("LEXARII_BRANDING_DIR", "")
	if cfg.BrandingDir != "" {
		// Checked now rather than at the first visitor: a mistyped path would
		// otherwise only show as a sign-in page that looks like the default.
		if info, err := os.Stat(cfg.BrandingDir); err != nil {
			fail("LEXARII_BRANDING_DIR: %v", err)
		} else if !info.IsDir() {
			fail("LEXARII_BRANDING_DIR (%s) is not a directory", cfg.BrandingDir)
		}
	}

	cfg.MaxUploadBytes = int64(envInt("LEXARII_MAX_UPLOAD_MB", 2048, fail)) << 20
	cfg.UploadPolicy = UploadPolicy(strings.ToLower(strings.TrimSpace(
		env("LEXARII_UPLOAD_POLICY", string(UploadAdmins)))))
	if !cfg.UploadPolicy.valid() {
		fail("LEXARII_UPLOAD_POLICY must be %q or %q, not %q",
			UploadAdmins, UploadUsers, cfg.UploadPolicy)
	}
	cfg.PeopleDirectory = PeopleDirectory(strings.ToLower(strings.TrimSpace(
		env("LEXARII_PEOPLE_DIRECTORY", string(DirectoryAll)))))
	if !cfg.PeopleDirectory.valid() {
		fail("LEXARII_PEOPLE_DIRECTORY must be %q or %q, not %q",
			DirectoryAll, DirectoryNone, cfg.PeopleDirectory)
	}
	cfg.DeletedAccountFiles = DeletedAccountFiles(strings.ToLower(strings.TrimSpace(
		env("LEXARII_DELETED_ACCOUNT_FILES", string(FilesArchive)))))
	if !cfg.DeletedAccountFiles.valid() {
		fail("LEXARII_DELETED_ACCOUNT_FILES must be %q or %q, not %q",
			FilesArchive, FilesErase, cfg.DeletedAccountFiles)
	}
	if cfg.UploadDir == "" {
		cfg.UploadDir = filepath.Join(cfg.DataDir, "uploads")
	}

	if cfg.DatabaseURL == "" {
		fail("DATABASE_URL is required")
	}
	// Uploads must never land inside the read-only library directory.
	if within(cfg.LibraryDir, cfg.UploadDir) {
		fail("LEXARII_UPLOAD_DIR (%s) is inside LEXARII_LIBRARY_DIR (%s); "+
			"the library directory is read-only and uploads must be stored elsewhere",
			cfg.UploadDir, cfg.LibraryDir)
	}
	// Nor the other way round: Lexarii deletes from its upload directory and
	// its archive - a replaced copy, a file removed for everyone - and a
	// library directory inside either would be deleted from.
	if within(cfg.UploadDir, cfg.LibraryDir) {
		fail("LEXARII_LIBRARY_DIR (%s) is inside LEXARII_UPLOAD_DIR (%s); "+
			"Lexarii deletes files from its upload directory, and the library directory is read-only",
			cfg.LibraryDir, cfg.UploadDir)
	}
	if archive := cfg.ArchiveDir(); within(archive, cfg.LibraryDir) || within(cfg.LibraryDir, archive) {
		fail("LEXARII_LIBRARY_DIR (%s) and the archive (%s, inside LEXARII_DATA_DIR) overlap; "+
			"Lexarii moves files into and deletes files from its archive, and the library directory is read-only",
			cfg.LibraryDir, archive)
	}
	// A half-configured issuer is the state that produces a confusing failure at
	// the moment someone tries to sign in.
	if cfg.OIDC.Issuer != "" && cfg.OIDC.ClientID == "" {
		fail("LEXARII_OIDC_CLIENT_ID is required when LEXARII_OIDC_ISSUER is set")
	}
	if cfg.OIDC.ClientID != "" && cfg.OIDC.Issuer == "" {
		fail("LEXARII_OIDC_ISSUER is required when LEXARII_OIDC_CLIENT_ID is set")
	}

	if cfg.LDAP.Enabled() && cfg.LDAP.BaseDN == "" && cfg.LDAP.UserDNTemplate == "" {
		fail("LEXARII_LDAP_BASE_DN or LEXARII_LDAP_USER_DN_TEMPLATE is required when LEXARII_LDAP_URL is set")
	}

	// Refused rather than defaulted: a proxy provider with no trusted addresses
	// lets anyone who can reach the port assert any identity.
	if cfg.Proxy.Enabled() && len(cfg.Proxy.TrustedProxies) == 0 {
		fail("LEXARII_PROXY_TRUSTED is required when LEXARII_PROXY_USER_HEADER is set; " +
			"without it anyone who can reach this server can claim to be anyone")
	}

	if cfg.WorkerCount < 1 {
		fail("LEXARII_WORKER_COUNT must be at least 1, got %d", cfg.WorkerCount)
	}
	// A configured model must be fully configured. Half-configured is the state
	// that produces confusing failures an hour into an enrichment run.
	if cfg.LLM.Enabled() && cfg.LLM.Model == "" {
		fail("LEXARII_LLM_MODEL is required when LEXARII_LLM_BASE_URL is set")
	}
	if cfg.Embed.Enabled() && cfg.Embed.Model == "" {
		fail("LEXARII_EMBED_MODEL is required when LEXARII_EMBED_BASE_URL is set")
	}
	if cfg.Embed.Enabled() && cfg.Embed.Dimensions < 1 {
		fail("LEXARII_EMBED_DIMENSIONS must be positive, got %d", cfg.Embed.Dimensions)
	}
	// Refused rather than raised quietly: somebody who set a day meant a day,
	// and should hear why they cannot have it.
	if cfg.JobRetention < domain.MinJobRetention {
		fail("LEXARII_JOB_RETENTION must be at least %s (enrichment reads a week of finished jobs back), got %s",
			domain.MinJobRetention, cfg.JobRetention)
	}

	if len(errs) > 0 {
		return Config{}, errors.Join(errs...)
	}
	return cfg, nil
}

func env(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && strings.TrimSpace(v) != "" {
		return strings.TrimSpace(v)
	}
	return def
}

func envInt(key string, def int, fail func(string, ...any)) int {
	raw := env(key, "")
	if raw == "" {
		return def
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		fail("%s must be an integer, got %q", key, raw)
		return def
	}
	return n
}

// envBool reads true or false, in any case. Anything else is refused rather
// than read as false: "yes" or "1" meaning off would be a silent surprise.
func envBool(key string, def bool, fail func(string, ...any)) bool {
	raw := env(key, "")
	switch strings.ToLower(raw) {
	case "":
		return def
	case "true":
		return true
	case "false":
		return false
	}
	fail("%s must be true or false, got %q", key, raw)
	return def
}

func envDuration(key string, def time.Duration, fail func(string, ...any)) time.Duration {
	raw := env(key, "")
	if raw == "" {
		return def
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		fail("%s must be a duration such as 90s or 2m, got %q", key, raw)
		return def
	}
	return d
}

// ArchiveDir is where the books of departed holders are kept, inside the data
// directory.
func (c Config) ArchiveDir() string { return filepath.Join(c.DataDir, "archive") }

// within reports whether path lies inside root. Used to keep uploads out of the
// read-only library directory.
func within(root, path string) bool {
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return false
	}
	absPath, err := filepath.Abs(path)
	if err != nil {
		return false
	}
	rel, err := filepath.Rel(absRoot, absPath)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// splitList parses a comma or space separated list, returning nil when empty so
// a caller's default applies.
func splitList(raw string) []string {
	fields := strings.FieldsFunc(raw, func(r rune) bool {
		return r == ',' || r == ' '
	})
	if len(fields) == 0 {
		return nil
	}
	return fields
}

// AdminAccount is the first administrator, named in the environment.
type AdminAccount struct {
	Username string
	Password string
	Email    string
}

// Wanted reports a configured bootstrap account.
func (a AdminAccount) Wanted() bool {
	return a.Username != "" && a.Password != ""
}
