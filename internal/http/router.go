// SPDX-License-Identifier: AGPL-3.0-or-later

package http

import (
	"net/http"

	"scm.poniatowski.dev/adam/lexarii/internal/auth"
)

// Router assembles the full HTTP surface. Handlers take their dependencies
// from structs; there are no package-level globals.
type Router struct {
	Health      *HealthHandler
	Library     *LibraryHandler
	Jobs        *JobHandler
	Uploads     *UploadHandler
	Reader      *ReaderHandler
	Progress    *ProgressHandler
	Sync        *SyncHandler
	OPDS        *OPDSHandler
	Auth        *AuthHandler
	Annotations *AnnotationHandler
	Enrichment  *EnrichmentHandler
	Ask         *AskHandler
	Chat        *ChatHandler
	Sharing     *SharingHandler
	Covers      *CoverHandler
	Accounts    *AccountHandler
	Citations   *CitationHandler
	Collections *CollectionHandler
	Moderation  *ModerationHandler
	// Sessions enforces authentication. When nil the server runs open, which is
	// only appropriate for a test.
	Sessions *auth.Sessions
	// Proxy authenticates from a trusted reverse proxy's headers, when one is
	// configured.
	Proxy *auth.ProxyProvider
	// Authorize, when set, applies the installation's own access policy to
	// every protected request once its user is known. Nil applies none. See
	// AuthorizeFunc. Like authentication itself it needs Sessions: without
	// them nothing is protected and there is no user to ask about.
	Authorize AuthorizeFunc
	// Branding, when set, serves the installation's own branding at
	// GET /branding/{path...}: normally a *BrandingFiles over
	// LEXARII_BRANDING_DIR, or an embedder's own handler. It is outside /api,
	// so it is open like the rest of the web client - the sign-in page is
	// what it is for. Serving it does not tell the client to load it; that is
	// AuthHandler.Branding, which an embedder serving /branding/ in front of
	// this router sets on its own.
	Branding http.Handler
}

// Handler returns the configured mux, using Go 1.22+ method-and-pattern routing.
func (rt *Router) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.Handle("GET /api/v1/health", rt.Health)

	if rt.Library != nil {
		mux.HandleFunc("GET /api/v1/library", rt.Library.List)
		mux.HandleFunc("GET /api/v1/stats", rt.Library.Stats)
		mux.HandleFunc("GET /api/v1/subjects", rt.Library.Subjects)
		mux.HandleFunc("GET /api/v1/works/{id}", rt.Library.Detail)
		mux.HandleFunc("GET /api/v1/works/{id}/mentions", rt.Library.Mentions)
		// Correcting the library by hand. What is corrected here is recorded
		// as the user's, which is what stops the next scan undoing it.
		mux.HandleFunc("PATCH /api/v1/works/{id}", rt.Library.Edit)
		// One book that arrived twice under two filenames. Found, never
		// merged automatically.
		mux.HandleFunc("GET /api/v1/duplicates", rt.Library.Duplicates)
		mux.HandleFunc("POST /api/v1/works/{id}/merge", rt.Library.Merge)
	}

	if rt.Jobs != nil {
		mux.HandleFunc("POST /api/v1/scan", rt.Jobs.StartScan)
		mux.HandleFunc("GET /api/v1/jobs/{id}", rt.Jobs.Get)
		mux.HandleFunc("DELETE /api/v1/jobs/{id}", rt.Jobs.Cancel)
		mux.HandleFunc("GET /api/v1/jobs/{id}/events", rt.Jobs.Events)
	}

	if rt.Reader != nil {
		mux.HandleFunc("GET /api/v1/books/{id}/info", rt.Reader.Info)
		mux.HandleFunc("GET /api/v1/books/{id}/file", rt.Reader.File)
		mux.HandleFunc("GET /api/v1/books/{id}/manifest", rt.Reader.Manifest)
		mux.HandleFunc("GET /api/v1/books/{id}/spine/{index}", rt.Reader.Content)
		mux.HandleFunc("GET /api/v1/books/{id}/assets/{path...}", rt.Reader.Asset)
		// One page of a PDF. Deliberately not behind the download permission: a
		// book shared to be read has to be readable.
		mux.HandleFunc("GET /api/v1/books/{id}/page/{n}", rt.Reader.Page)
	}

	if rt.Auth != nil {
		mux.HandleFunc("GET /api/v1/auth/providers", rt.Auth.ProvidersList)
		mux.HandleFunc("POST /api/v1/auth/login", rt.Auth.Login)
		// A client that holds its own token, rather than a browser holding a
		// cookie. Separate endpoint so no web page can be made to ask for one.
		mux.HandleFunc("POST /api/v1/auth/token", rt.Auth.Token)
		mux.HandleFunc("POST /api/v1/auth/logout", rt.Auth.Logout)
		mux.HandleFunc("POST /api/v1/auth/setup", rt.Auth.Setup)
		mux.HandleFunc("GET /api/v1/auth/me", rt.Auth.Me)
		mux.HandleFunc("GET /api/v1/auth/sessions", rt.Auth.SessionList)
		mux.HandleFunc("DELETE /api/v1/auth/sessions/{id}", rt.Auth.RevokeSession)
		mux.HandleFunc("POST /api/v1/auth/sessions/revoke-all", rt.Auth.RevokeAllSessions)
		// Federated sign-in. The path carries the provider name, so adding one
		// needs no new route.
		mux.HandleFunc("GET /api/v1/auth/{provider}/start", rt.Auth.StartRedirect)
		mux.HandleFunc("GET /api/v1/auth/{provider}/callback", rt.Auth.Callback)
	}

	if rt.Annotations != nil {
		mux.HandleFunc("GET /api/v1/books/{id}/annotations", rt.Annotations.List)
		mux.HandleFunc("POST /api/v1/books/{id}/annotations", rt.Annotations.Create)
		mux.HandleFunc("POST /api/v1/books/{id}/annotations/orphaned", rt.Annotations.Orphan)
		mux.HandleFunc("POST /api/v1/books/{id}/annotations/import", rt.Annotations.Import)
		mux.HandleFunc("PATCH /api/v1/annotations/{id}", rt.Annotations.Update)
		mux.HandleFunc("DELETE /api/v1/annotations/{id}", rt.Annotations.Delete)
		mux.HandleFunc("GET /api/v1/annotations", rt.Annotations.Recent)
	}

	// The catalogue every ebook reader already speaks. Not under /api/v1: an
	// OPDS address is pasted into a reader app by a person, and /opds is the
	// one they expect and the one every other library server uses.
	if rt.OPDS != nil {
		mux.HandleFunc("GET /opds", rt.OPDS.Root)
		mux.HandleFunc("GET /opds/new", rt.OPDS.New)
		mux.HandleFunc("GET /opds/all", rt.OPDS.All)
		mux.HandleFunc("GET /opds/search", rt.OPDS.Search)
		mux.HandleFunc("GET /opds/search.xml", rt.OPDS.SearchDescription)
		mux.HandleFunc("GET /opds/subjects", rt.OPDS.Subjects)
		mux.HandleFunc("GET /opds/subjects/{slug}", rt.OPDS.Subject)
		mux.HandleFunc("GET /opds/collections", rt.OPDS.Collections)
		mux.HandleFunc("GET /opds/collections/{id}", rt.OPDS.Collection)
	}

	if rt.Sync != nil {
		// One endpoint, two directions. A client pulls what changed and pushes
		// what it did; everything else it needs - the library, the books
		// themselves - it already has endpoints for.
		mux.HandleFunc("GET /api/v1/sync", rt.Sync.Pull)
		mux.HandleFunc("POST /api/v1/sync", rt.Sync.Push)
	}

	if rt.Progress != nil {
		mux.HandleFunc("GET /api/v1/books/{id}/progress", rt.Progress.Get)
		mux.HandleFunc("PUT /api/v1/books/{id}/progress", rt.Progress.Put)
		mux.HandleFunc("GET /api/v1/continue", rt.Progress.Continue)
	}

	if rt.Ask != nil {
		mux.HandleFunc("POST /api/v1/ask", rt.Ask.Ask)
	}

	if rt.Chat != nil {
		mux.HandleFunc("GET /api/v1/conversations", rt.Chat.List)
		mux.HandleFunc("POST /api/v1/conversations", rt.Chat.Post)
		mux.HandleFunc("GET /api/v1/conversations/{id}", rt.Chat.Get)
		mux.HandleFunc("DELETE /api/v1/conversations/{id}", rt.Chat.Delete)
		mux.HandleFunc("POST /api/v1/conversations/{id}/messages", rt.Chat.Post)
		mux.HandleFunc("POST /api/v1/conversations/stream", rt.Chat.Stream)
		mux.HandleFunc("POST /api/v1/conversations/{id}/messages/stream", rt.Chat.Stream)
	}

	if rt.Enrichment != nil {
		mux.HandleFunc("POST /api/v1/enrich", rt.Enrichment.Start)
		mux.HandleFunc("POST /api/v1/index", rt.Enrichment.StartIndexing)
		mux.HandleFunc("GET /api/v1/suggestions", rt.Enrichment.Suggestions)
		mux.HandleFunc("POST /api/v1/suggestions/{id}/accept", rt.Enrichment.Resolve(true))
		mux.HandleFunc("POST /api/v1/suggestions/{id}/reject", rt.Enrichment.Resolve(false))
	}

	if rt.Covers != nil {
		mux.HandleFunc("GET /api/v1/covers/pending", rt.Covers.Pending)
		mux.HandleFunc("GET /api/v1/books/{id}/cover", rt.Covers.Get)
		mux.HandleFunc("PUT /api/v1/books/{id}/cover", rt.Covers.Put)
	}

	if rt.Collections != nil {
		mux.HandleFunc("GET /api/v1/collections", rt.Collections.List)
		mux.HandleFunc("POST /api/v1/collections", rt.Collections.Create)
		mux.HandleFunc("GET /api/v1/collections/{id}", rt.Collections.Detail)
		mux.HandleFunc("PATCH /api/v1/collections/{id}", rt.Collections.Update)
		mux.HandleFunc("DELETE /api/v1/collections/{id}", rt.Collections.Delete)
		mux.HandleFunc("PUT /api/v1/collections/{id}/works/{workId}", rt.Collections.Add)
		mux.HandleFunc("DELETE /api/v1/collections/{id}/works/{workId}", rt.Collections.Remove)
	}

	if rt.Citations != nil {
		mux.HandleFunc("GET /api/v1/works/{id}/citation", rt.Citations.Work)
		mux.HandleFunc("GET /api/v1/citations", rt.Citations.Bibliography)
	}

	if rt.Sharing != nil {
		mux.HandleFunc("GET /api/v1/people", rt.Sharing.People)
		mux.HandleFunc("GET /api/v1/shares/activity", rt.Sharing.Activity)

		// Deleting an account, and the books it leaves behind. Not under the
		// admin guard as a group: deleting YOUR OWN account needs no
		// administrator, and the store draws that line itself.
		mux.HandleFunc("DELETE /api/v1/users/{id}", rt.Accounts.Delete)
		mux.HandleFunc("POST /api/v1/admin/users/{id}/erase-files", rt.Accounts.EraseFiles)
		mux.HandleFunc("GET /api/v1/archive", rt.Accounts.Archived)
		mux.HandleFunc("POST /api/v1/archive/{id}/claim", rt.Accounts.Claim)
		// Taking a book off your own shelf. Always the caller's own holding;
		// nobody, administrators included, ends anybody else's here. Taking a
		// file off the installation for everybody is a different act, under
		// /admin.
		mux.HandleFunc("DELETE /api/v1/books/{id}/holding", rt.Accounts.ReleaseHolding)
		// Sharing YOUR OWN notes on a book, which is not the owner's decision:
		// these sit under annotations rather than under the book's shares.
		mux.HandleFunc("GET /api/v1/books/{id}/annotations/shares", rt.Sharing.NoteShares)
		mux.HandleFunc("POST /api/v1/books/{id}/annotations/shares", rt.Sharing.PostNoteShareByUsername)
		mux.HandleFunc("PUT /api/v1/books/{id}/annotations/shares/{userId}", rt.Sharing.PutNoteShare)
		mux.HandleFunc("DELETE /api/v1/books/{id}/annotations/shares/{userId}", rt.Sharing.DeleteNoteShare)
		mux.HandleFunc("GET /api/v1/books/{id}/shares", rt.Sharing.List)
		mux.HandleFunc("POST /api/v1/books/{id}/shares", rt.Sharing.PostByUsername)
		mux.HandleFunc("PUT /api/v1/books/{id}/shares/{userId}", rt.Sharing.Put)
		mux.HandleFunc("DELETE /api/v1/books/{id}/shares/{userId}", rt.Sharing.Delete)
		mux.HandleFunc("GET /api/v1/books/{id}/access", rt.Sharing.AccessLog)
	}

	if rt.Uploads != nil {
		mux.HandleFunc("POST /api/v1/uploads", rt.Uploads.Create)
	}

	if rt.Moderation != nil {
		// Taking a file off the installation for everybody, and the blocklist
		// that keeps it off. Administrators only, every one of them.
		mux.HandleFunc("POST /api/v1/admin/books/{id}/remove", rt.Moderation.Remove)
		mux.HandleFunc("GET /api/v1/admin/blocked", rt.Moderation.Blocked)
		mux.HandleFunc("DELETE /api/v1/admin/blocked/{hash}", rt.Moderation.Unblock)
		mux.HandleFunc("POST /api/v1/admin/blocked/{hash}/discard", rt.Moderation.DiscardCopies)
	}

	if rt.Branding != nil {
		mux.Handle("GET /branding/{path...}", rt.Branding)
	}

	// The web client is mounted last, on the root pattern, so every API route
	// above takes precedence.
	if client, err := clientHandler(); err == nil {
		mux.Handle("GET /", client)
	}

	var handler http.Handler = mux
	if rt.Sessions != nil {
		// Innermost, so every route above is behind it. The auth handler is
		// passed as well, because it is what can check the username and
		// password an ebook reader sends - the only credentials those know
		// how to offer.
		var basic BasicAuthenticator
		if rt.Auth != nil {
			basic = rt.Auth
		}
		handler = RequireAuthWithPolicy(rt.Sessions, basic, rt.Authorize, handler)

		// Header authentication wraps that, because it must establish a session
		// before the session check runs.
		if rt.Proxy != nil && rt.Auth != nil {
			handler = HeaderAuth(rt.Proxy, rt.Auth.Store, rt.Sessions, rt.Auth.logger(), handler)
		}
	}
	return Recover(LogRequests(SecureHeaders(handler)))
}
