// SPDX-License-Identifier: AGPL-3.0-or-later

package config

import (
	"os"
	"regexp"
	"strings"
	"testing"
	"time"
)

// load runs Load with a clean environment plus the given variables, so one
// test's setting cannot reach another and the developer's own shell cannot
// reach any of them.
func load(t *testing.T, vars map[string]string) (Config, error) {
	t.Helper()
	for _, kv := range os.Environ() {
		if name, _, ok := strings.Cut(kv, "="); ok {
			if strings.HasPrefix(name, "LEXARII_") || name == "DATABASE_URL" {
				t.Setenv(name, "")
			}
		}
	}
	// DATABASE_URL is the one required variable; a test about anything else
	// should not have to say so.
	if _, ok := vars["DATABASE_URL"]; !ok {
		t.Setenv("DATABASE_URL", "postgres://localhost/lexarii")
	}
	for k, v := range vars {
		t.Setenv(k, v)
	}
	return Load()
}

// The defaults are the configuration almost every installation runs, so they
// are worth pinning. The three that are security decisions rather than taste
// are the reason this test exists: each one is safe-by-default and opted out
// of, and a flip in either direction should fail here rather than in somebody
// else's library.
func TestTheDefaultsAreTheSafeOnes(t *testing.T) {
	cfg, err := load(t, nil)
	if err != nil {
		t.Fatalf("loading with only DATABASE_URL: %v", err)
	}

	if !cfg.SecureCookies {
		t.Error("session cookies are not Secure by default")
	}
	// Plain LDAP carries the password. Upgrading is the default and turning it
	// off is deliberate; this was the other way round, and disagreed with what
	// .env.example told everybody it was.
	if !cfg.LDAP.StartTLS {
		t.Error("LDAP StartTLS is off by default, so passwords cross the wire in the clear")
	}
	if cfg.LDAP.InsecureSkipVerify {
		t.Error("LDAP certificate verification is disabled by default")
	}
	if cfg.OIDC.AllowSignup || cfg.LDAP.AllowSignup {
		t.Error("a federated provider creates accounts by default, letting anyone with " +
			"an account at the issuer into the library")
	}
	if !cfg.AutoMigrate {
		t.Error("migrations do not run by default, so a first start finds no schema")
	}
	// An existing installation must not start letting every account upload
	// because it upgraded.
	if cfg.UploadPolicy != UploadAdmins || cfg.UploadPolicy.AnyoneMayUpload() {
		t.Errorf("UploadPolicy = %q, want %q", cfg.UploadPolicy, UploadAdmins)
	}
	// Nor stop listing people to each other.
	if cfg.PeopleDirectory != DirectoryAll || !cfg.PeopleDirectory.Listed() {
		t.Errorf("PeopleDirectory = %q, want %q", cfg.PeopleDirectory, DirectoryAll)
	}
	// Nor start erasing what people leave behind.
	if cfg.DeletedAccountFiles != FilesArchive || cfg.DeletedAccountFiles.Erase() {
		t.Errorf("DeletedAccountFiles = %q, want %q", cfg.DeletedAccountFiles, FilesArchive)
	}

	if cfg.Addr != ":8080" {
		t.Errorf("Addr = %q", cfg.Addr)
	}
	if cfg.WorkerCount != 4 {
		t.Errorf("WorkerCount = %d", cfg.WorkerCount)
	}
	if cfg.LLM.ContextTokens != 8192 {
		t.Errorf("LLM.ContextTokens = %d, want the 8192 an 8k local model has",
			cfg.LLM.ContextTokens)
	}
	if cfg.Embed.Dimensions != 768 {
		t.Errorf("Embed.Dimensions = %d, and the schema stores 768", cfg.Embed.Dimensions)
	}
	if cfg.MaxUploadBytes != 2048<<20 {
		t.Errorf("MaxUploadBytes = %d", cfg.MaxUploadBytes)
	}

	// No model, no catalogue, no public address. Every one of these is a
	// promise: the library works with nothing configured, and nothing leaves
	// the machine until somebody says so.
	if cfg.LLM.Enabled() || cfg.Embed.Enabled() || cfg.Rerank.Enabled() {
		t.Error("a model endpoint is configured by default")
	}
	if cfg.MetadataBaseURL != "" {
		t.Errorf("MetadataBaseURL = %q, want blank: asking a catalogue sends a "+
			"description of somebody's shelf to a third party", cfg.MetadataBaseURL)
	}
	if cfg.PublicURL != "" {
		t.Errorf("PublicURL = %q, want blank rather than a guess", cfg.PublicURL)
	}
	if cfg.Admin.Wanted() {
		t.Error("an administrator account is created by default")
	}
}

// A proxy provider trusts a header. A header is only a credential because of
// where it came from, so the list of addresses allowed to send it is not
// optional and not defaultable.
func TestAProxyProviderWithoutTrustedAddressesIsRefused(t *testing.T) {
	_, err := load(t, map[string]string{
		"LEXARII_PROXY_USER_HEADER": "X-Forwarded-User",
	})
	if err == nil {
		t.Fatal("a proxy provider with no trusted addresses was accepted; anyone " +
			"who can reach the port can claim to be anyone")
	}
	if !strings.Contains(err.Error(), "LEXARII_PROXY_TRUSTED") {
		t.Errorf("error does not name the variable to set: %v", err)
	}
}

// The other three proxy headers default to what the common proxies send, so a
// working setup is one variable. They must stay inert until the switch is on.
func TestTheProxyHeaderDefaultsDoNothingOnTheirOwn(t *testing.T) {
	cfg, err := load(t, nil)
	if err != nil {
		t.Fatalf("loading: %v", err)
	}
	if cfg.Proxy.Enabled() {
		t.Fatal("the proxy provider is enabled with no user header set")
	}
	if cfg.Proxy.EmailHeader != "X-Forwarded-Email" {
		t.Errorf("EmailHeader = %q, want oauth2-proxy's name", cfg.Proxy.EmailHeader)
	}
	if cfg.Proxy.GroupsHeader != "X-Forwarded-Groups" {
		t.Errorf("GroupsHeader = %q", cfg.Proxy.GroupsHeader)
	}
}

// Half-configured is the state that fails at the moment somebody tries to sign
// in, which is the worst possible moment to discover it.
func TestHalfConfiguredProvidersAreRefused(t *testing.T) {
	for _, tc := range []struct {
		name string
		vars map[string]string
		want string
	}{
		{"an issuer with no client", map[string]string{
			"LEXARII_OIDC_ISSUER": "https://issuer.example",
		}, "LEXARII_OIDC_CLIENT_ID"},
		{"a client with no issuer", map[string]string{
			"LEXARII_OIDC_CLIENT_ID": "lexarii",
		}, "LEXARII_OIDC_ISSUER"},
		{"a directory with nowhere to look", map[string]string{
			"LEXARII_LDAP_URL": "ldap://dc.example",
		}, "LEXARII_LDAP_BASE_DN"},
		{"a chat endpoint with no model named", map[string]string{
			"LEXARII_LLM_BASE_URL": "http://localhost:8081/v1",
		}, "LEXARII_LLM_MODEL"},
		{"an embedding endpoint with no model named", map[string]string{
			"LEXARII_EMBED_BASE_URL": "http://localhost:8082/v1",
		}, "LEXARII_EMBED_MODEL"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := load(t, tc.vars)
			if err == nil {
				t.Fatalf("accepted %s", tc.name)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error does not name %s: %v", tc.want, err)
			}
		})
	}
}

// Invariant 1: the library directory is read-only. Uploads have to go
// somewhere, and that somewhere is never inside it.
func TestUploadsCannotBeStoredInsideTheLibrary(t *testing.T) {
	_, err := load(t, map[string]string{
		"LEXARII_LIBRARY_DIR": "/srv/books",
		"LEXARII_UPLOAD_DIR":  "/srv/books/incoming",
	})
	if err == nil {
		t.Fatal("uploads were accepted inside the read-only library directory")
	}
}

// The uploads directory defaults under the DATA directory, not the library
// one, and that is the whole point of it being separate.
func TestUploadsDefaultUnderTheDataDirectory(t *testing.T) {
	cfg, err := load(t, map[string]string{"LEXARII_DATA_DIR": "/var/lib/lexarii"})
	if err != nil {
		t.Fatalf("loading: %v", err)
	}
	if cfg.UploadDir != "/var/lib/lexarii/uploads" {
		t.Errorf("UploadDir = %q", cfg.UploadDir)
	}
}

// A typo in a number must stop the process with the name of the variable, not
// silently become a default that behaves differently.
func TestAMalformedValueNamesItsVariable(t *testing.T) {
	for name, value := range map[string]string{
		"LEXARII_WORKER_COUNT":       "four",
		"LEXARII_LLM_TIMEOUT":        "two minutes",
		"LEXARII_MAX_UPLOAD_MB":      "2gb",
		"LEXARII_ARCHIVE_RETENTION":  "6 months",
		"LEXARII_LLM_CONTEXT_TOKENS": "8k",
		"LEXARII_JOB_RETENTION":      "a month",
	} {
		t.Run(name, func(t *testing.T) {
			_, err := load(t, map[string]string{name: value})
			if err == nil {
				t.Fatalf("accepted %s=%q", name, value)
			}
			if !strings.Contains(err.Error(), name) {
				t.Errorf("error does not name the variable: %v", err)
			}
		})
	}
}

func TestUploadPolicy(t *testing.T) {
	for _, tc := range []struct {
		value   string
		want    UploadPolicy
		anyone  bool
		invalid bool
	}{
		{value: "", want: UploadAdmins},
		{value: "admins", want: UploadAdmins},
		{value: "users", want: UploadUsers, anyone: true},
		// Read the way people write it in an env file.
		{value: " Users ", want: UploadUsers, anyone: true},
		{value: "everyone", invalid: true},
		{value: "user", invalid: true},
		{value: "true", invalid: true},
	} {
		t.Run(tc.value, func(t *testing.T) {
			cfg, err := load(t, map[string]string{"LEXARII_UPLOAD_POLICY": tc.value})
			if tc.invalid {
				if err == nil {
					t.Fatalf("accepted LEXARII_UPLOAD_POLICY=%q", tc.value)
				}
				if !strings.Contains(err.Error(), "LEXARII_UPLOAD_POLICY") {
					t.Errorf("error does not name the variable: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("loading: %v", err)
			}
			if cfg.UploadPolicy != tc.want {
				t.Errorf("UploadPolicy = %q, want %q", cfg.UploadPolicy, tc.want)
			}
			if cfg.UploadPolicy.AnyoneMayUpload() != tc.anyone {
				t.Errorf("AnyoneMayUpload = %v, want %v", cfg.UploadPolicy.AnyoneMayUpload(), tc.anyone)
			}
		})
	}
}

func TestPeopleDirectory(t *testing.T) {
	for _, tc := range []struct {
		value   string
		want    PeopleDirectory
		listed  bool
		invalid bool
	}{
		{value: "", want: DirectoryAll, listed: true},
		{value: "all", want: DirectoryAll, listed: true},
		{value: "none", want: DirectoryNone},
		{value: " None ", want: DirectoryNone},
		{value: "admins", invalid: true},
		{value: "false", invalid: true},
		{value: "off", invalid: true},
	} {
		t.Run(tc.value, func(t *testing.T) {
			cfg, err := load(t, map[string]string{"LEXARII_PEOPLE_DIRECTORY": tc.value})
			if tc.invalid {
				if err == nil {
					t.Fatalf("accepted LEXARII_PEOPLE_DIRECTORY=%q", tc.value)
				}
				if !strings.Contains(err.Error(), "LEXARII_PEOPLE_DIRECTORY") {
					t.Errorf("error does not name the variable: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("loading: %v", err)
			}
			if cfg.PeopleDirectory != tc.want || cfg.PeopleDirectory.Listed() != tc.listed {
				t.Errorf("PeopleDirectory = %q (listed %v), want %q (listed %v)",
					cfg.PeopleDirectory, cfg.PeopleDirectory.Listed(), tc.want, tc.listed)
			}
		})
	}
}

func TestDeletedAccountFiles(t *testing.T) {
	for _, tc := range []struct {
		value   string
		want    DeletedAccountFiles
		erase   bool
		invalid bool
	}{
		{value: "", want: FilesArchive},
		{value: "archive", want: FilesArchive},
		{value: "erase", want: FilesErase, erase: true},
		{value: " Erase ", want: FilesErase, erase: true},
		{value: "delete", invalid: true},
		{value: "true", invalid: true},
		{value: "keep", invalid: true},
	} {
		t.Run(tc.value, func(t *testing.T) {
			cfg, err := load(t, map[string]string{"LEXARII_DELETED_ACCOUNT_FILES": tc.value})
			if tc.invalid {
				if err == nil {
					t.Fatalf("accepted LEXARII_DELETED_ACCOUNT_FILES=%q", tc.value)
				}
				if !strings.Contains(err.Error(), "LEXARII_DELETED_ACCOUNT_FILES") {
					t.Errorf("error does not name the variable: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("loading: %v", err)
			}
			if cfg.DeletedAccountFiles != tc.want || cfg.DeletedAccountFiles.Erase() != tc.erase {
				t.Errorf("DeletedAccountFiles = %q (erase %v), want %q (erase %v)",
					cfg.DeletedAccountFiles, cfg.DeletedAccountFiles.Erase(), tc.want, tc.erase)
			}
		})
	}
}

func TestWorkerCountMustBeAtLeastOne(t *testing.T) {
	if _, err := load(t, map[string]string{"LEXARII_WORKER_COUNT": "0"}); err == nil {
		t.Fatal("zero workers was accepted, so nothing would ever be ingested")
	}
}

// Durations and byte sizes are parsed, not guessed at.
func TestSizesAndDurationsAreParsed(t *testing.T) {
	cfg, err := load(t, map[string]string{
		"LEXARII_LLM_TIMEOUT":       "90s",
		"LEXARII_ARCHIVE_RETENTION": "720h",
		"LEXARII_MAX_UPLOAD_MB":     "512",
		"LEXARII_PDF_CACHE_MB":      "256",
	})
	if err != nil {
		t.Fatalf("loading: %v", err)
	}
	if cfg.LLM.Timeout != 90*time.Second {
		t.Errorf("LLM.Timeout = %v", cfg.LLM.Timeout)
	}
	if cfg.ArchiveRetention != 720*time.Hour {
		t.Errorf("ArchiveRetention = %v", cfg.ArchiveRetention)
	}
	if cfg.MaxUploadBytes != 512<<20 {
		t.Errorf("MaxUploadBytes = %d", cfg.MaxUploadBytes)
	}
	if cfg.PDFCacheBytes != 256<<20 {
		t.Errorf("PDFCacheBytes = %d", cfg.PDFCacheBytes)
	}
}

// Finished jobs are kept 30 days unless told otherwise, and never less than
// the week enrichment reads back: a shorter retention stops startup, naming
// the variable, rather than being quietly raised.
func TestJobRetention(t *testing.T) {
	for _, tc := range []struct {
		value string
		want  time.Duration
		ok    bool
	}{
		{"", 30 * 24 * time.Hour, true},
		{"2160h", 2160 * time.Hour, true},
		{"168h", 168 * time.Hour, true},
		{"24h", 0, false},
		{"-1h", 0, false},
	} {
		t.Run(tc.value, func(t *testing.T) {
			cfg, err := load(t, map[string]string{"LEXARII_JOB_RETENTION": tc.value})
			if !tc.ok {
				if err == nil || !strings.Contains(err.Error(), "LEXARII_JOB_RETENTION") {
					t.Fatalf("LEXARII_JOB_RETENTION=%q: err = %v, want one naming the variable", tc.value, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("loading: %v", err)
			}
			if cfg.JobRetention != tc.want {
				t.Errorf("JobRetention = %v, want %v", cfg.JobRetention, tc.want)
			}
		})
	}
}

// Trailing slashes are somebody typing a URL the way a browser shows it, and
// they would otherwise produce //v1 and //api paths.
func TestTrailingSlashesAreTrimmedFromURLs(t *testing.T) {
	cfg, err := load(t, map[string]string{
		"LEXARII_PUBLIC_URL":        "https://books.example/",
		"LEXARII_METADATA_BASE_URL": "https://openlibrary.org/",
		"LEXARII_RERANK_BASE_URL":   "http://localhost:8083/v1/",
	})
	if err != nil {
		t.Fatalf("loading: %v", err)
	}
	if cfg.PublicURL != "https://books.example" {
		t.Errorf("PublicURL = %q", cfg.PublicURL)
	}
	if cfg.MetadataBaseURL != "https://openlibrary.org" {
		t.Errorf("MetadataBaseURL = %q", cfg.MetadataBaseURL)
	}
	if cfg.Rerank.BaseURL != "http://localhost:8083/v1" {
		t.Errorf("Rerank.BaseURL = %q", cfg.Rerank.BaseURL)
	}
}

// A variable set to whitespace is a variable somebody meant to leave blank,
// most often an empty line in a compose file.
func TestABlankValueMeansUnset(t *testing.T) {
	cfg, err := load(t, map[string]string{"LEXARII_ADDR": "   "})
	if err != nil {
		t.Fatalf("loading: %v", err)
	}
	if cfg.Addr != ":8080" {
		t.Errorf("Addr = %q, want the default", cfg.Addr)
	}
}

func TestScopesAndTrustedListsSplitOnCommasAndSpaces(t *testing.T) {
	cfg, err := load(t, map[string]string{
		"LEXARII_OIDC_ISSUER":       "https://issuer.example",
		"LEXARII_OIDC_CLIENT_ID":    "lexarii",
		"LEXARII_OIDC_SCOPES":       "openid, profile email",
		"LEXARII_PROXY_USER_HEADER": "X-Forwarded-User",
		"LEXARII_PROXY_TRUSTED":     "10.0.0.0/8 127.0.0.1",
	})
	if err != nil {
		t.Fatalf("loading: %v", err)
	}
	if got := strings.Join(cfg.OIDC.Scopes, "|"); got != "openid|profile|email" {
		t.Errorf("Scopes = %q", got)
	}
	if len(cfg.Proxy.TrustedProxies) != 2 {
		t.Errorf("TrustedProxies = %v", cfg.Proxy.TrustedProxies)
	}
}

var envVar = regexp.MustCompile(`LEXARII_[A-Z0-9_]+`)

// .env.example is the only documentation most people will read, and it drifts
// silently: a variable added to the code and not to the file is one nobody
// finds, and a variable in the file that the code stopped reading is a setting
// somebody will change and then wonder why nothing happened.
//
// Six variables had drifted out of the file when this was written -
// AUTO_MIGRATE, EMBED_TIMEOUT, LDAP_INSECURE_SKIP_VERIFY, LDAP_LABEL,
// LDAP_USER_DN_TEMPLATE and PROXY_LABEL - including the one that turns off
// certificate verification.
func TestEveryVariableIsDocumentedAndEveryDocumentedVariableIsRead(t *testing.T) {
	source, err := os.ReadFile("config.go")
	if err != nil {
		t.Fatalf("reading config.go: %v", err)
	}
	example, err := os.ReadFile("../../.env.example")
	if err != nil {
		t.Fatalf("reading .env.example: %v", err)
	}

	read := map[string]bool{}
	for _, name := range envVar.FindAllString(string(source), -1) {
		read[name] = true
	}
	documented := map[string]bool{}
	for _, name := range envVar.FindAllString(string(example), -1) {
		documented[name] = true
	}

	for name := range read {
		if !documented[name] {
			t.Errorf("%s is read by config.go and appears nowhere in .env.example", name)
		}
	}
	for name := range documented {
		if !read[name] {
			t.Errorf("%s is in .env.example and is not read by config.go", name)
		}
	}
}

// Indexing on ingest is opt-in, and a value that is not plainly true or false
// is refused by name rather than quietly read as off.
func TestIndexOnIngest(t *testing.T) {
	cfg, err := load(t, nil)
	if err != nil {
		t.Fatalf("loading: %v", err)
	}
	if cfg.IndexOnIngest {
		t.Error("indexing on ingest is on by default")
	}

	for raw, want := range map[string]bool{"true": true, "TRUE": true, "false": false, " False ": false} {
		cfg, err := load(t, map[string]string{"LEXARII_INDEX_ON_INGEST": raw})
		if err != nil {
			t.Fatalf("%q: %v", raw, err)
		}
		if cfg.IndexOnIngest != want {
			t.Errorf("%q: IndexOnIngest = %v, want %v", raw, cfg.IndexOnIngest, want)
		}
	}

	for _, raw := range []string{"yes", "1", "on", "tru"} {
		_, err := load(t, map[string]string{"LEXARII_INDEX_ON_INGEST": raw})
		if err == nil || !strings.Contains(err.Error(), "LEXARII_INDEX_ON_INGEST") {
			t.Errorf("%q: err = %v, want a refusal naming LEXARII_INDEX_ON_INGEST", raw, err)
		}
	}
}

// Enrichment on ingest is opt-in in the same way, and refused by name when it
// is neither true nor false.
func TestEnrichOnIngest(t *testing.T) {
	cfg, err := load(t, nil)
	if err != nil {
		t.Fatalf("loading: %v", err)
	}
	if cfg.EnrichOnIngest {
		t.Error("enrichment on ingest is on by default")
	}

	for raw, want := range map[string]bool{"true": true, "TRUE": true, "false": false, " False ": false} {
		cfg, err := load(t, map[string]string{"LEXARII_ENRICH_ON_INGEST": raw})
		if err != nil {
			t.Fatalf("%q: %v", raw, err)
		}
		if cfg.EnrichOnIngest != want {
			t.Errorf("%q: EnrichOnIngest = %v, want %v", raw, cfg.EnrichOnIngest, want)
		}
	}

	for _, raw := range []string{"yes", "1", "on", "tru"} {
		_, err := load(t, map[string]string{"LEXARII_ENRICH_ON_INGEST": raw})
		if err == nil || !strings.Contains(err.Error(), "LEXARII_ENRICH_ON_INGEST") {
			t.Errorf("%q: err = %v, want a refusal naming LEXARII_ENRICH_ON_INGEST", raw, err)
		}
	}
}

// Nor may the library directory sit inside a directory Lexarii deletes from:
// its uploads, or its archive - nor the archive inside the library.
func TestTheLibraryCannotOverlapWhatLexariiDeletesFrom(t *testing.T) {
	for name, vars := range map[string]map[string]string{
		"library inside uploads": {"LEXARII_LIBRARY_DIR": "/srv/up/books", "LEXARII_UPLOAD_DIR": "/srv/up",
			"LEXARII_DATA_DIR": "/srv/data"},
		"library inside the archive": {"LEXARII_LIBRARY_DIR": "/srv/data/archive/books", "LEXARII_DATA_DIR": "/srv/data"},
		"the archive inside the library": {"LEXARII_LIBRARY_DIR": "/srv/books", "LEXARII_DATA_DIR": "/srv/books/.lexarii",
			"LEXARII_UPLOAD_DIR": "/srv/uploads"},
	} {
		if _, err := load(t, vars); err == nil || !strings.Contains(err.Error(), "LEXARII_LIBRARY_DIR") {
			t.Errorf("%s: err = %v, want a refusal naming LEXARII_LIBRARY_DIR", name, err)
		}
	}
	if _, err := load(t, map[string]string{"LEXARII_LIBRARY_DIR": "/srv/books", "LEXARII_DATA_DIR": "/srv/data"}); err != nil {
		t.Errorf("separate directories refused: %v", err)
	}
}
