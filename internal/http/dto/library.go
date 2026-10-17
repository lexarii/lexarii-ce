// SPDX-License-Identifier: AGPL-3.0-or-later

// Package dto holds the wire types for the HTTP API.
//
// These are deliberately separate from the domain types. The wire format is a
// contract with the client and changes on its own schedule; letting handlers
// serialise domain structs directly turns every internal rename into a breaking
// API change.
//
// Conventions, applied without exception: camelCase field names, RFC3339
// timestamps, UUID strings, cursor pagination.
package dto

import (
	"encoding/json"
	"time"
)

// LibraryItem is one card in the library grid.
type LibraryItem struct {
	WorkID      string   `json:"workId"`
	Title       string   `json:"title"`
	Subtitle    string   `json:"subtitle,omitempty"`
	Authors     []string `json:"authors"`
	Series      string   `json:"series,omitempty"`
	SeriesIndex *float64 `json:"seriesIndex,omitempty"`
	Formats     []string `json:"formats"`
	Tags        []string `json:"tags,omitempty"`
	CoverURL    string   `json:"coverUrl,omitempty"`
	// UnitCount is pages for a PDF and spine items for an EPUB. These are not
	// the same thing - an EPUB with 104 spine items is a book of several hundred
	// pages - so UnitType says which, and the UI must not label either "pages"
	// unconditionally.
	UnitCount *int   `json:"unitCount,omitempty"`
	UnitType  string `json:"unitType,omitempty"`
	Status    string `json:"status"`
	// Missing means the file is absent from disk - an unmounted drive or a
	// deletion. The row is kept so annotations and reading position survive.
	Missing    bool      `json:"missing"`
	BookFileID string    `json:"bookFileId"`
	AddedAt    time.Time `json:"addedAt"`
	// Progress is how far this reader has got, 0 to 1, absent if they have not
	// opened it. The furthest point reached rather than the current position:
	// a shelf should say how much of a book has been read, not where someone
	// happens to be after turning back to check something.
	Progress *float32 `json:"progress,omitempty"`
	// LentBy names whoever owns this book, when that is not the person looking
	// at it. Absent for your own books, which is nearly all of them.
	LentBy string `json:"lentBy,omitempty"`
	// CanDownload is false for a book shared to be read rather than kept. The
	// shelf uses it to say so before somebody goes looking for a download that
	// is not there.
	CanDownload bool `json:"canDownload"`
}

// LibraryPage is a page of library items.
type LibraryPage struct {
	Items []LibraryItem `json:"items"`
	// NextCursor is empty when there are no further pages.
	NextCursor string `json:"nextCursor,omitempty"`
}

// Author is a contributor on a work.
type Author struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	SortName string `json:"sortName"`
	Role     string `json:"role"`
}

// BookFile is one file backing a work.
type BookFile struct {
	ID          string `json:"id"`
	Format      string `json:"format"`
	SizeBytes   int64  `json:"sizeBytes"`
	Status      string `json:"status"`
	IngestError string `json:"ingestError,omitempty"`
	// UnitCount is pages for a PDF, spine items for an EPUB. See UnitType.
	UnitCount   *int       `json:"unitCount,omitempty"`
	UnitType    string     `json:"unitType,omitempty"`
	Missing     bool       `json:"missing"`
	ExtractedAt *time.Time `json:"extractedAt,omitempty"`
	// Path is the location on the server's disk. Shown so an administrator
	// can find the file; never written to. Empty for anybody who is not an
	// administrator, and for a file the viewer did not scan themselves: one
	// lent to them, or one they uploaded, which carries Filename instead.
	Path string `json:"path"`
	// Filename is the name this viewer gave the file when they added it.
	// Empty for a file they do not hold, or one found by a scan, whose path
	// already says what it is called. Uploads are stored under their content
	// hash, so this is the only place an uploaded file's own name appears, and
	// it is the viewer's: two people can hold one file under different names.
	Filename string `json:"filename,omitempty"`
	// Held says whether this viewer holds the file - added it themselves - as
	// opposed to having it lent to them. Only a holder can take it off their
	// shelf (DELETE /books/{id}/holding). Omitted when false.
	Held bool `json:"held,omitempty"`
	// CanDownload says whether this viewer may have the file itself. A book
	// can be lent to be read without being handed over, so the download is
	// offered only where it would actually work.
	CanDownload bool `json:"canDownload"`
}

// Tag is a label on a work.
type Tag struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Slug      string `json:"slug"`
	IsSubject bool   `json:"isSubject"`
	// Source says where the tag came from, so the UI can mark machine-generated
	// tags and let the user reject them.
	Source string `json:"source,omitempty"`
}

// WorkDetail is the book detail page.
type WorkDetail struct {
	ID          string   `json:"id"`
	Title       string   `json:"title"`
	Subtitle    string   `json:"subtitle,omitempty"`
	SortTitle   string   `json:"sortTitle"`
	Series      string   `json:"series,omitempty"`
	SeriesIndex *float64 `json:"seriesIndex,omitempty"`
	Language    string   `json:"language,omitempty"`
	Description string   `json:"description,omitempty"`
	Summary     string   `json:"summary,omitempty"`
	Authors     []Author `json:"authors"`
	// Editions carry the publisher, the year and the ISBNs. Written at ingest
	// and, until this page could be corrected, never sent to the client at
	// all - so a book's page showed a title and an author while the record
	// held the rest.
	Editions []Edition  `json:"editions,omitempty"`
	Files    []BookFile `json:"files"`
	Tags     []Tag      `json:"tags,omitempty"`
	// Warnings are recoverable problems found while parsing: a missing cover, an
	// unreadable TOC, an ignored producer artefact.
	Warnings []string `json:"warnings,omitempty"`
	// Provenance maps a field name to where its value came from: embedded, user,
	// llm, filename, external. The UI uses this to show what was guessed.
	Provenance map[string]string `json:"provenance,omitempty"`
	CreatedAt  time.Time         `json:"createdAt"`
	UpdatedAt  time.Time         `json:"updatedAt"`
}

// Stats summarises the library.
type Stats struct {
	Works    int            `json:"works"`
	Files    int            `json:"files"`
	Authors  int            `json:"authors"`
	Tags     int            `json:"tags"`
	Pages    int            `json:"pages"`
	Missing  int            `json:"missing"`
	NeedsOCR int            `json:"needsOcr"`
	Failed   int            `json:"failed"`
	ByFormat map[string]int `json:"byFormat"`
	// QueueBusy is the number of pending and running jobs. Non-zero means the
	// library is still filling up.
	QueueBusy int `json:"queueBusy"`
}

// JobAccepted is the response to a request that starts background work.
type JobAccepted struct {
	JobID string `json:"jobId"`
	// Created is false when an equivalent job was already queued, in which case
	// JobID names that one. Asking twice is not an error.
	Created bool `json:"created"`
}

// Job is the state of one background job.
type Job struct {
	ID           string     `json:"id"`
	Type         string     `json:"type"`
	Status       string     `json:"status"`
	Progress     float32    `json:"progress"`
	ProgressNote string     `json:"progressNote,omitempty"`
	Attempts     int        `json:"attempts"`
	MaxAttempts  int        `json:"maxAttempts"`
	LastError    string     `json:"lastError,omitempty"`
	CreatedAt    time.Time  `json:"createdAt"`
	RunAfter     *time.Time `json:"runAfter,omitempty"`
}

// UploadStatus is the outcome of one uploaded file.
type UploadStatus string

const (
	// UploadStored means the bytes are on disk and the book is queued.
	UploadStored UploadStatus = "stored"
	// UploadDuplicate means Lexarii already had this exact content. Nothing
	// was written and nothing needs doing; re-uploading a batch is safe.
	UploadDuplicate UploadStatus = "duplicate"
	// UploadRejected means the file was not accepted, with a reason.
	UploadRejected UploadStatus = "rejected"
)

// UploadedFile is the per-file outcome of a bulk upload.
type UploadedFile struct {
	Filename    string       `json:"filename"`
	Status      UploadStatus `json:"status"`
	ContentHash string       `json:"contentHash,omitempty"`
	Format      string       `json:"format,omitempty"`
	SizeBytes   int64        `json:"sizeBytes,omitempty"`
	// JobID names the extraction job, when one was queued.
	JobID string `json:"jobId,omitempty"`
	Error string `json:"error,omitempty"`
	// Code says why a rejected file was rejected, when a client may want to
	// branch on it: "content_blocked" for bytes an administrator has removed
	// from the installation. Empty otherwise.
	Code string `json:"code,omitempty"`
	Note string `json:"note,omitempty"`
}

// UploadSummary counts the outcomes.
type UploadSummary struct {
	Stored    int `json:"stored"`
	Duplicate int `json:"duplicate"`
	Rejected  int `json:"rejected"`
}

// UploadResult is the response to a bulk upload.
//
// Every file is reported individually and independently. A bulk upload over a
// flaky connection will lose files partway through, and the client has to be
// able to show exactly which ones landed: the recovery is to re-upload the
// batch, which deduplication makes cheap.
type UploadResult struct {
	Files   []UploadedFile `json:"files"`
	Summary UploadSummary  `json:"summary"`
	// Interrupted is true when the request ended before every part was read.
	// The listed files are still stored.
	Interrupted bool   `json:"interrupted,omitempty"`
	Message     string `json:"message,omitempty"`
}

// SpineItem is one document in reading order.
type SpineItem struct {
	Index int    `json:"index"`
	ID    string `json:"id,omitempty"`
	// Linear is false for supplementary content a reader may skip. It keeps its
	// index regardless, because dropping it would renumber every locator after it.
	Linear bool `json:"linear"`
}

// TOCEntry is one navigation point.
type TOCEntry struct {
	Title string `json:"title"`
	// SpineIndex is where the entry points: the spine index for EPUB, the PAGE
	// for PDF - the same thing `major` means in a locator for each format.
	//
	// -1 when it resolves to neither, which happens in real books and is not an
	// error. A PDF destination in particular may name a page in another file or
	// nothing at all, and an entry pointing at the wrong page would be worse
	// than one pointing nowhere, because the reader trusts it.
	SpineIndex int        `json:"spineIndex"`
	Fragment   string     `json:"fragment,omitempty"`
	Children   []TOCEntry `json:"children,omitempty"`
}

// ReaderManifest is everything the reader needs before it opens a book.
type ReaderManifest struct {
	BookFileID string      `json:"bookFileId"`
	Format     string      `json:"format"`
	Spine      []SpineItem `json:"spine"`
	TOC        []TOCEntry  `json:"toc"`
}

// SpineContent is one document, sanitised and ready to insert into the DOM.
type SpineContent struct {
	Index int `json:"index"`
	// HTML is a fragment, not a document: the reader supplies the page and the
	// typography. The book's own stylesheet is deliberately not included.
	HTML string `json:"html"`
}

// StartEnrichment optionally bounds one enrichment run.
type StartEnrichment struct {
	Limit int `json:"limit,omitempty"`
}

// EnrichmentStarted reports what was queued.
type EnrichmentStarted struct {
	// Queued counts jobs newly created. Eligible counts books that still need
	// describing, which is larger when some are already queued.
	Queued   int `json:"queued"`
	Eligible int `json:"eligible"`
	// PromptID is the prompt these jobs will use. Stored with every result, so
	// a library described by one prompt can be told from one described by the
	// next.
	PromptID string `json:"promptId"`
	// ModelConfigured is false when no model is set up at all, which is the
	// difference between "nothing happened" and "switch your model on".
	ModelConfigured bool `json:"modelConfigured"`
}

// AskRequest is a question put to the library.
type AskRequest struct {
	Question string `json:"question"`
	// BookFileID pins the question to one book, for asking about what you are
	// reading rather than about everything.
	BookFileID string `json:"bookFileId,omitempty"`
}

// Source is one passage an answer was built from.
type Source struct {
	// Number is the citation marker in the answer: [1], [2].
	Number int `json:"number"`
	// Cited is false for a passage that was retrieved but not referred to. It
	// is still shown, because it is part of why the answer says what it says.
	Cited bool `json:"cited"`

	WorkID   string `json:"workId"`
	BookFile string `json:"bookFileId"`
	Title    string `json:"title"`
	// Label is the readable source: the book, the chapter, the page.
	Label   string `json:"label"`
	Chapter string `json:"chapter,omitempty"`
	// Locator opens the reader at this passage.
	Locator string `json:"locator"`
	Excerpt string `json:"excerpt"`

	Score float64 `json:"score"`
	// FoundBy is "meaning", "keyword", or "both" - which arm of the search
	// found this passage. Useful when an answer is wrong.
	FoundBy string `json:"foundBy,omitempty"`

	// Kind is "passage" or "note". A note is something the reader wrote in
	// the margin of one of these books, and showing it as though the book had
	// said it would misattribute their own opinion to an author.
	Kind string `json:"kind,omitempty"`
}

// AnswerDelta is one piece of a streaming answer.
//
// A bare string would be simpler on the wire and worse to live with: every
// other event in the stream is a JSON object, and a client that has to special
// case one of them will get it wrong.
type AnswerDelta struct {
	Text string `json:"text"`
}

// Answer is a question, what was done with it, and what it produced.
type Answer struct {
	Question string `json:"question"`
	// Answer is empty when no chat model is configured, in which case the
	// sources are the whole result: search with citations.
	Answer string `json:"answer"`

	// Intent is what the router decided the question was. Shown because it
	// explains why a question returned a reading list rather than an answer.
	Intent   string   `json:"intent"`
	Subjects []string `json:"subjects,omitempty"`
	// SearchedFor is what was actually searched for, which is usually the
	// question and is one entry per side for a comparison. Plural because a
	// comparison is retrieved once per side, and reporting only the first
	// would describe half of what happened.
	SearchedFor []string `json:"searchedFor,omitempty"`

	Sources []Source `json:"sources"`
	// Books answers a question that wanted a reading list rather than a
	// synthesis: "what should I read about X". Empty for other intents.
	Books []AnswerBook `json:"books"`

	Model        string `json:"model,omitempty"`
	PromptID     string `json:"promptId,omitempty"`
	RetrievalMS  int    `json:"retrievalMs"`
	GenerationMS int    `json:"generationMs"`

	// Set when the question was asked inside a conversation.
	ConversationID string `json:"conversationId,omitempty"`
	MessageID      string `json:"messageId,omitempty"`
}

// ConversationSummary is one thread in a list.
type ConversationSummary struct {
	ID        string    `json:"id"`
	Title     string    `json:"title"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// ConversationList is a user's threads, most recently used first.
type ConversationList struct {
	Items []ConversationSummary `json:"items"`
}

// Message is one turn in a conversation.
type Message struct {
	ID      string `json:"id"`
	Seq     int    `json:"seq"`
	Role    string `json:"role"`
	Content string `json:"content"`

	// How the answer was produced, so a disappointing one can be diagnosed
	// rather than guessed at.
	Intent       string `json:"intent,omitempty"`
	Model        string `json:"model,omitempty"`
	RetrievalMS  int    `json:"retrievalMs,omitempty"`
	GenerationMS int    `json:"generationMs,omitempty"`
	// Unsupported marks an answer given with no passages behind it.
	Unsupported bool `json:"unsupported,omitempty"`

	Citations []Source  `json:"citations"`
	CreatedAt time.Time `json:"createdAt"`
}

// Conversation is a thread and its turns.
type Conversation struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	// BookFileID is set when the thread is pinned to one book.
	BookFileID string    `json:"bookFileId,omitempty"`
	Messages   []Message `json:"messages"`
	CreatedAt  time.Time `json:"createdAt"`
	UpdatedAt  time.Time `json:"updatedAt"`
}

// AnswerBook is one book in a reading list.
type AnswerBook struct {
	WorkID  string   `json:"workId"`
	Title   string   `json:"title"`
	Authors []string `json:"authors,omitempty"`
}

// IndexingStarted reports what was queued and how much of the library is
// searchable.
type IndexingStarted struct {
	Queued   int `json:"queued"`
	Eligible int `json:"eligible"`
	// StrategyVersion is how these chunks are being made. Changing it makes
	// every existing chunk stale rather than silently mixed with new ones.
	StrategyVersion int `json:"strategyVersion"`

	// Files is every book with a text layer; IndexedFiles is how many have
	// passages; EmbeddedChunks is how many of those can be searched by meaning.
	Files          int `json:"files"`
	IndexedFiles   int `json:"indexedFiles"`
	Chunks         int `json:"chunks"`
	EmbeddedChunks int `json:"embeddedChunks"`

	// ModelConfigured is false when no embedding model is set up, which is the
	// difference between "nothing happened" and "switch your model on".
	ModelConfigured bool `json:"modelConfigured"`
}

// Suggestion is something a model proposed, awaiting a person's decision.
type Suggestion struct {
	ID        string `json:"id"`
	WorkID    string `json:"workId"`
	WorkTitle string `json:"workTitle"`
	// Field is what is being proposed: title, summary, series, subjects,
	// audience.
	Field string `json:"field"`
	// Value is the proposal itself, shaped by the field.
	Value json.RawMessage `json:"value"`
	// Current is what the work holds now, so the two can be compared. Empty
	// when the field has no value, which is the case the proposal fills.
	Current string `json:"current,omitempty"`
	// Model and PromptID are the provenance: which model, on which prompt.
	// Shown to the user, because "the AI said so" is not a reason on its own.
	Model       string    `json:"model,omitempty"`
	PromptID    string    `json:"promptId,omitempty"`
	Confidence  *float64  `json:"confidence,omitempty"`
	GeneratedAt time.Time `json:"generatedAt"`
}

// SuggestionList is everything awaiting review.
type SuggestionList struct {
	Items []Suggestion `json:"items"`
}

// Session is one live sign-in, as shown to the person who owns it.
type Session struct {
	ID string `json:"id"`
	// Current marks the session making this request, so a person can tell which
	// row is the device in their hand before revoking one of the others.
	Current bool `json:"current"`
	// UserAgent is the browser's own claim about itself. It is not verified and
	// is shown only as a hint for recognising a device.
	UserAgent string `json:"userAgent,omitempty"`
	// ClientIP is the socket address the session was created from.
	ClientIP string `json:"clientIp,omitempty"`
	// Provider is how this session was authenticated: local, oidc, ldap, proxy.
	Provider string `json:"provider,omitempty"`
	// Device is set when this session is a bearer token held by a native
	// client, and names it. Empty means a browser session in a cookie.
	Device     string    `json:"device,omitempty"`
	CreatedAt  time.Time `json:"createdAt"`
	LastUsedAt time.Time `json:"lastUsedAt"`
	ExpiresAt  time.Time `json:"expiresAt"`
}

// SessionList is every session that can still be used.
type SessionList struct {
	Items []Session `json:"items"`
}

// ReadingProgress is where a user has read to in one file.
type ReadingProgress struct {
	BookFileID string `json:"bookFileId"`
	// Started is false when the book has never been opened, which is not an
	// error and must not be treated as one.
	Started bool `json:"started"`
	// Locator is the canonical position string, and is where the reader reopens.
	// It is independent of font size, viewport, and device: a percentage would
	// survive none of those.
	Locator string `json:"locator,omitempty"`
	// Percent renders Locator. Display only, never used to restore a position.
	Percent *float32 `json:"percent,omitempty"`
	// FurthestLocator is the deepest position reached, which may be ahead of
	// Locator when the reader has turned back. It is what a "how far through
	// am I?" figure is measured from, and it never moves backwards.
	FurthestLocator string     `json:"furthestLocator,omitempty"`
	FurthestPercent *float32   `json:"furthestPercent,omitempty"`
	LastReadAt      *time.Time `json:"lastReadAt,omitempty"`
}

// SaveProgress is the request body for recording a position.
type SaveProgress struct {
	Locator string   `json:"locator"`
	Percent *float32 `json:"percent,omitempty"`
}

// ContinueItem is a book the user has started reading.
type ContinueItem struct {
	WorkID     string    `json:"workId"`
	Title      string    `json:"title"`
	BookFileID string    `json:"bookFileId"`
	Format     string    `json:"format"`
	Percent    *float32  `json:"percent,omitempty"`
	LastReadAt time.Time `json:"lastReadAt"`
}

// ContinueList is the "continue reading" shelf.
type ContinueList struct {
	Items []ContinueItem `json:"items"`
}

// BookInfo tells the reader which surface to open before it fetches anything
// heavy.
type BookInfo struct {
	BookFileID string `json:"bookFileId"`
	// Title and WorkID let the reader name the book it is showing and link
	// back to it. Without them the chrome could only count spine items.
	Title     string `json:"title"`
	WorkID    string `json:"workId"`
	Format    string `json:"format"`
	SizeBytes int64  `json:"sizeBytes"`
	Status    string `json:"status"`
	UnitCount *int   `json:"unitCount,omitempty"`
	// Reflowable is true for formats whose text is re-laid-out to the reader's
	// typography. A PDF has fixed pages and is rendered as-is.
	Reflowable bool `json:"reflowable"`
	// CanDownload says whether this person may take the original file. False
	// for a book shared to read, in which case a PDF reader must fetch pages
	// one at a time rather than the whole document.
	CanDownload bool `json:"canDownload"`
}

// AuthProvider is one configured sign-in method.
type AuthProvider struct {
	Name        string `json:"name"`
	DisplayName string `json:"displayName"`
	// Kind tells the client how to present it: "password" renders a form,
	// "redirect" renders a button, "header" is handled by a proxy.
	Kind string `json:"kind"`
}

// AuthOptions is what the login screen needs to render itself.
type AuthOptions struct {
	Providers []AuthProvider `json:"providers"`
	// SetupRequired is true when no account exists yet, so the client offers
	// account creation rather than a login form nobody can satisfy.
	SetupRequired bool `json:"setupRequired"`
	// Links are the administrator's own links for the sign-in page - how to
	// ask for an account, where to reset a password, the terms of use - from
	// LEXARII_SIGNIN_LINKS. Always present, empty when none are configured.
	Links []SignInLink `json:"links"`
	// Branding is true when the installation serves its own stylesheet at
	// /branding/branding.css, which the client then loads after its own
	// styles. From LEXARII_BRANDING_DIR, or set by an embedder serving
	// /branding/ itself.
	Branding bool `json:"branding"`
}

// SignInLink is one link shown on the sign-in page.
type SignInLink struct {
	Label string `json:"label"`
	// Href is a path on this server, beginning with a single "/", or an
	// https:// URL. Nothing else is accepted when the server starts.
	Href string `json:"href"`
}

// LoginRequest signs in with a password provider.
type LoginRequest struct {
	Provider string `json:"provider,omitempty"`
	Username string `json:"username"`
	Password string `json:"password"`
}

// TokenRequest asks for a bearer token for a client that is not a browser.
type TokenRequest struct {
	Provider string `json:"provider,omitempty"`
	Username string `json:"username"`
	Password string `json:"password"`
	// Device names the thing the token is for - "Adam's phone" - and is
	// required. It is how this token is recognised in the session list and
	// revoked, months later, from a device that is no longer to hand.
	Device string `json:"device"`
}

// AuthToken is a bearer token and the account it signs in as.
//
// The token is shown ONCE. Only its hash is stored, so a lost token is
// replaced rather than recovered.
type AuthToken struct {
	Token string `json:"token"`
	// SessionID is the same id the session list shows, so a client can offer
	// to revoke its own session.
	SessionID string    `json:"sessionId"`
	ExpiresAt time.Time `json:"expiresAt"`
	User      User      `json:"user"`
}

// SetupRequest creates the first account.
type SetupRequest struct {
	Username string `json:"username"`
	Email    string `json:"email,omitempty"`
	Password string `json:"password"`
}

// User is the signed-in account.
type User struct {
	ID       string `json:"id"`
	Username string `json:"username"`
	Email    string `json:"email,omitempty"`
	IsAdmin  bool   `json:"isAdmin"`
	// CanUpload says whether this person may add books by uploading them:
	// administrators always, everybody else when the installation's upload
	// policy allows it. The server decides either way; this is so a client
	// offers the button only where pressing it would work. Omitted when false,
	// so a client built against this field still reads an older server.
	CanUpload bool `json:"canUpload,omitempty"`
	// CanAsk says whether asking the library can work here: a chat model is
	// configured. Without one every question fails, so a client hides its
	// Ask entry points rather than offering a box that cannot answer. It says
	// nothing about whether the model is up right now - that changes by the
	// minute, and an answer that fails says so. Omitted when false.
	CanAsk bool `json:"canAsk,omitempty"`
	// CanListPeople says whether GET /people lists every account for this
	// person: always for an administrator, and for everybody else unless the
	// installation keeps its people unlisted (LEXARII_PEOPLE_DIRECTORY=none),
	// when a client asks for a recipient's username instead of offering a
	// list. Omitted when false.
	CanListPeople bool      `json:"canListPeople,omitempty"`
	CreatedAt     time.Time `json:"createdAt"`
}

// Annotation is a highlight, note, or bookmark.
type Annotation struct {
	ID         string `json:"id"`
	BookFileID string `json:"bookFileId"`
	Type       string `json:"type"`
	// Start and End are canonical locator strings. They anchor to the document's
	// structure, so they survive a font-size change; they do not survive the
	// document itself changing, which is what Orphaned reports.
	Start string `json:"start"`
	End   string `json:"end"`
	// SelectedText is the text as it read when the mark was made. It is what an
	// orphaned annotation is shown with, so a note is never lost in silence.
	SelectedText string `json:"selectedText,omitempty"`
	Note         string `json:"note,omitempty"`
	Colour       string `json:"colour,omitempty"`
	// Orphaned means the anchor no longer resolves. The mark is kept and shown,
	// never deleted.
	Orphaned bool `json:"orphaned"`
	// Shared marks somebody else's note, visible because they shared it. The
	// store has always known - it is how a borrowed book can be argued with -
	// but the wire dropped it, so the reader painted another person's highlight
	// as though it were the reader's own and offered to edit it. Shared marks
	// are readable and never editable.
	Shared bool `json:"shared,omitempty"`
	// Author names who wrote it, for a shared mark. Empty for your own.
	Author    string    `json:"author,omitempty"`
	CreatedAt time.Time `json:"createdAt"`
	// UpdatedAt is when this server wrote the row - an edit, a sync push, or
	// re-ingest orphaning it. It is not when the change was made; it is when
	// this server heard about it, which is what a sync pull's `since` reads.
	UpdatedAt time.Time `json:"updatedAt"`
	// EditedAt is when the change actually IN this row was made, by whichever
	// device made it - the value sync's last-write-wins compares. A client
	// reconciling a pulled mark against an edit it made offline and has not
	// pushed yet must compare against THIS, not UpdatedAt: two edits made
	// seconds apart on different devices can arrive at the server hours
	// apart, and UpdatedAt would say the later ARRIVAL won rather than the
	// later EDIT.
	//
	// Absent when the server does not know it, never the zero time: a mark
	// never edited since it was made carries its creation time here, and a
	// client seeing no value falls back to UpdatedAt. Year one once went out
	// on the notes view and read as the oldest edit there could be.
	EditedAt *time.Time `json:"editedAt,omitempty"`
}

// AnnotationList is every mark in one book.
type AnnotationList struct {
	Items []Annotation `json:"items"`
}

// CreateAnnotation makes a new mark.
type CreateAnnotation struct {
	Type         string `json:"type"`
	Start        string `json:"start"`
	End          string `json:"end"`
	SelectedText string `json:"selectedText,omitempty"`
	Note         string `json:"note,omitempty"`
	Colour       string `json:"colour,omitempty"`
}

// UpdateAnnotation edits a mark's note or colour. The anchor is deliberately
// not editable: moving a highlight to different text is a different highlight.
type UpdateAnnotation struct {
	Note   string `json:"note"`
	Colour string `json:"colour,omitempty"`
}

// OrphanAnnotations reports what the reader found when it tried to resolve a
// chapter's anchors.
//
// Both directions are reported. An anchor that starts resolving again - after a
// file is restored, or a deterministic re-ingest - must be able to lose the
// flag, or a transient failure marks a note as orphaned for good.
type OrphanAnnotations struct {
	IDs []string `json:"ids"`
	// Resolved lists marks whose anchors were found, so a stale flag clears.
	Resolved []string `json:"resolved,omitempty"`
}

// OrphanResult reports how many marks were flagged.
type OrphanResult struct {
	Orphaned int `json:"orphaned"`
}

// RecentAnnotation is a mark alongside the book it belongs to.
type RecentAnnotation struct {
	Annotation Annotation `json:"annotation"`
	BookFileID string     `json:"bookFileId"`
	WorkID     string     `json:"workId"`
	Title      string     `json:"title"`
	Format     string     `json:"format"`
}

// RecentAnnotationList is the notes view across the whole library.
type RecentAnnotationList struct {
	Items []RecentAnnotation `json:"items"`
	// NextCursor continues the list from the last item; empty on the last
	// page.
	NextCursor string `json:"nextCursor,omitempty"`
}

// ImportedMark is one highlight read out of a book's own file by the client.
type ImportedMark struct {
	// Page is the physical page, 1-based.
	Page int `json:"page"`
	// Occurrence disambiguates a phrase that appears more than once on the
	// page, as a mark made in the reader does.
	Occurrence int    `json:"occurrence"`
	Text       string `json:"text"`
	// Note is whatever the other reader stored with it, usually nothing.
	Note string `json:"note,omitempty"`
}

// ImportMarks is a whole book's worth of them.
type ImportMarks struct {
	Marks []ImportedMark `json:"marks"`
}

// ImportResult says how many were kept.
type ImportResult struct {
	// Imported is how many were stored. Offered is how many were sent: a
	// rectangle over a picture has no words under it and is not a mark
	// anybody could be shown again.
	Imported int `json:"imported"`
	Offered  int `json:"offered"`
}

// Collection is a reader's own grouping of their books.
type Collection struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	// ParentID nests one collection inside another. Empty for a top-level one.
	ParentID string `json:"parentId,omitempty"`
	// Books is how many are in it.
	Books     int       `json:"books"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// CollectionList is every collection a person has made.
type CollectionList struct {
	Items []Collection `json:"items"`
}

// CollectionDetail is a collection and the books in it.
type CollectionDetail struct {
	Collection
	Items []LibraryItem `json:"items"`
}

// CollectionRequest creates or renames a collection.
type CollectionRequest struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	ParentID    string `json:"parentId,omitempty"`
}

// Error is the single error shape used by every endpoint.
//
// One shape everywhere, so a client has exactly one thing to parse when
// something goes wrong.
type Error struct {
	Error ErrorDetail `json:"error"`
}

// ErrorDetail is what went wrong.
type ErrorDetail struct {
	// Code is a stable machine-readable identifier. Clients may branch on it.
	Code string `json:"code"`
	// Message is for humans and may change freely.
	Message string `json:"message"`
	// Details carries whatever is specific to this failure.
	Details map[string]any `json:"details,omitempty"`
}

// Health is the answer to the open health endpoint.
//
// An anonymous caller - a load balancer, a container probe, anyone on the
// internet - gets Status alone. The rest is filled in only for an
// administrator, because it names internal hosts and carries raw error text,
// and because producing it is what costs outbound requests to model servers.
type Health struct {
	// Status is ok, degraded, or down. Degraded still answers 200: the
	// library and reader work with no model at all. Down answers 503.
	Status string `json:"status"`
	// Version is the build. Administrators only.
	Version string `json:"version,omitempty"`
	// Checks is one line per dependency, keyed by name. Administrators only.
	Checks map[string]string `json:"checks,omitempty"`
	// Degraded names the optional dependencies that are down. Administrators
	// only.
	Degraded []string `json:"degraded,omitempty"`
}

// Person is an account a book can be shared with.
//
// Username only. A share picker needs a name to point at; nothing about it
// needs an email address, and listing those to every account would hand out a
// contact list nobody agreed to.
type Person struct {
	ID       string `json:"id"`
	Username string `json:"username"`
	IsAdmin  bool   `json:"isAdmin"`
}

// PersonList is who a book can be shared with.
type PersonList struct {
	Items []Person `json:"items"`
}

// Share is one person's access to one book.
type Share struct {
	UserID   string `json:"userId"`
	Username string `json:"username"`
	// CanDownload allows taking the original file rather than only reading it.
	CanDownload bool `json:"canDownload"`
	// Annotations is true when the owner's highlights and notes travel with it.
	Annotations bool `json:"annotations"`
}

// ShareList is everyone a book is shared with.
type ShareList struct {
	Items []Share `json:"items"`
}

// ShareRequest sets one person's access to one book.
//
// Both default to false, which is the right way round: a share that silently
// permitted downloading, or quietly handed over the owner's marginalia, would
// be the surprising outcome.
type ShareRequest struct {
	CanDownload bool `json:"canDownload"`
	Annotations bool `json:"annotations"`
}

// ShareByUsername lends a book to the account with this username, for an
// installation that does not list its people to everybody. The name is
// matched without regard to case; the reply is the Share made, carrying the
// account's id for changing or ending it later.
type ShareByUsername struct {
	Username    string `json:"username"`
	CanDownload bool   `json:"canDownload"`
	Annotations bool   `json:"annotations"`
}

// NoteShareByUsername shares the caller's own marks on a book with the
// account with this username.
type NoteShareByUsername struct {
	Username string `json:"username"`
}

// Recipient is somebody a book or notes are shared with: an id to change or
// end the share by, and the name to show.
type Recipient struct {
	ID       string `json:"id"`
	Username string `json:"username"`
}

// AccessRecord is one person's reading of one book on one day.
//
// Only somebody ELSE'S reading is ever recorded: a person's own library is not
// logged, which is the difference between an audit trail and surveillance.
type AccessRecord struct {
	UserID   string `json:"userId"`
	Username string `json:"username"`
	Day      string `json:"day"`
	// Kind is page, chapter, or file.
	Kind string `json:"kind"`
	// Views counts requests; Units counts the DISTINCT pages or chapters seen.
	// The gap between them is re-reading, and Units against Of is how much of
	// the book this person has actually taken.
	Views int `json:"views"`
	Units int `json:"units"`
	Of    int `json:"of"`

	FirstAt time.Time `json:"firstAt"`
	LastAt  time.Time `json:"lastAt"`
}

// AccessLog is who has read a book.
type AccessLog struct {
	Items []AccessRecord `json:"items"`
}

// ShareActivity is one person's reading of one lent book on one day, named so
// the owner can tell which book it was without looking it up.
type ShareActivity struct {
	BookFileID string `json:"bookFileId"`
	WorkID     string `json:"workId"`
	WorkTitle  string `json:"workTitle"`
	UserID     string `json:"userId"`
	Username   string `json:"username"`
	Day        string `json:"day"`
	Kind       string `json:"kind"`
	Views      int    `json:"views"`
	Units      int    `json:"units"`
	Of         int    `json:"of"`
	// Notable marks reading worth interrupting somebody for: most of a book in
	// one day, or a whole-file download.
	Notable bool      `json:"notable"`
	FirstAt time.Time `json:"firstAt"`
	LastAt  time.Time `json:"lastAt"`
}

// Subject is one thing a library is about, and how much of it.
type Subject struct {
	Name string `json:"name"`
	// Slug is what the shelf filters on: /?tag=<slug>.
	Slug  string `json:"slug"`
	Books int    `json:"books"`
}

// SubjectList is everything this person's library is about, commonest first.
type SubjectList struct {
	Items []Subject `json:"items"`
}

// Mention is one book in this library naming another in its own text.
//
// A bibliography entry, an index line, a "further reading" note: found rather
// than declared, because nobody writes these down.
type Mention struct {
	// WorkID is the book doing the naming.
	WorkID     string   `json:"workId"`
	BookFileID string   `json:"bookFileId"`
	Title      string   `json:"title"`
	Authors    []string `json:"authors"`
	// Locator opens the reader where the name appears.
	Locator string `json:"locator"`
	Excerpt string `json:"excerpt"`
	// Places is how many passages of that book name this one.
	Places int `json:"places"`
}

// MentionList is every book found naming this one.
type MentionList struct {
	Items []Mention `json:"items"`
}

// Edition is one printing: a publisher, a year, an ISBN.
type Edition struct {
	ID        string `json:"id"`
	ISBN13    string `json:"isbn13,omitempty"`
	ISBN10    string `json:"isbn10,omitempty"`
	Publisher string `json:"publisher,omitempty"`
	// PublishedOn is a date because that is what the column holds, though for
	// almost every book only the year is known and it reads as 1 January.
	PublishedOn *time.Time `json:"publishedOn,omitempty"`
	PageCount   *int       `json:"pageCount,omitempty"`
	Language    string     `json:"language,omitempty"`
}

// DuplicateGroup is a set of works that look like one book.
type DuplicateGroup struct {
	// Reason says what the machine noticed, because merging is destructive
	// enough that somebody deciding should be told why they were asked.
	Reason string          `json:"reason"`
	Works  []DuplicateWork `json:"works"`
}

// DuplicateWork is one candidate, with enough to choose between them.
type DuplicateWork struct {
	WorkID  string   `json:"workId"`
	Title   string   `json:"title"`
	Authors []string `json:"authors,omitempty"`
	Formats []string `json:"formats,omitempty"`
	Files   int      `json:"files"`
	// Annotations and HasProgress say which copy has actually been read.
	// Nothing is lost by merging - marks follow their file - but the copy
	// somebody has read is usually the one they mean to keep.
	Annotations int  `json:"annotations"`
	HasProgress bool `json:"hasProgress"`
}

// DuplicateList is every group found.
type DuplicateList struct {
	Items []DuplicateGroup `json:"items"`
}

// MergeWorks names the books to fold into the one in the path.
type MergeWorks struct {
	Merge []string `json:"merge"`
}

// MergeResult reports what moved.
type MergeResult struct {
	Kept string `json:"kept"`
	// FilesMoved counts the editions repointed, which is how many separate
	// copies the kept book now holds.
	FilesMoved int `json:"filesMoved"`
}

// UpdateWork is a correction somebody made by hand.
//
// Every field is optional and the distinction matters: a field ABSENT is left
// as it is, a field present and empty is cleared. That is why these are
// pointers rather than strings - without it there is no way to say "remove the
// subtitle somebody's scanner invented", only "leave everything alone".
type UpdateWork struct {
	Title       *string  `json:"title,omitempty"`
	Subtitle    *string  `json:"subtitle,omitempty"`
	Series      *string  `json:"series,omitempty"`
	SeriesIndex *float64 `json:"seriesIndex,omitempty"`
	Language    *string  `json:"language,omitempty"`
	Description *string  `json:"description,omitempty"`
	// Authors replaces the whole list, in order. Replacing rather than adding
	// is the point: a book credited to a download site needs that name gone.
	Authors *[]string `json:"authors,omitempty"`

	Publisher *string `json:"publisher,omitempty"`
	// Year is the year of publication. Zero clears it.
	Year   *int    `json:"year,omitempty"`
	ISBN13 *string `json:"isbn13,omitempty"`
	ISBN10 *string `json:"isbn10,omitempty"`
}

// SyncPull is everything that changed for one reader since a moment.
//
// The shape a client catching up after a week offline receives.
type SyncPull struct {
	// Until is the SERVER's clock at the moment of THIS read. Store it as the
	// next `since` only once More comes back false and the round is done -
	// while paging with Cursor, every page's Until is a different, earlier
	// moment and none of them are the right one to keep. Never the client's
	// own clock: two clocks a minute apart would silently lose or repeat a
	// minute of somebody's reading.
	Until time.Time `json:"until"`
	// More says a page was filled and the rest is waiting. A client must page
	// through with Cursor - not by resending the same `since` - before using
	// Until, or it will skip what it did not receive.
	More bool `json:"more"`
	// Cursor resumes exactly where this page left off. Present whenever More
	// is true; send it back as the `cursor` query parameter on the next call.
	// Opaque - a client only ever echoes what it was given, never builds one.
	Cursor      string       `json:"cursor,omitempty"`
	Annotations []Annotation `json:"annotations"`
	// Deleted is the marks that are gone. A deletion cannot be inferred from
	// an absence, so it is stated.
	Deleted   []DeletedMark  `json:"deleted"`
	Positions []SyncPosition `json:"positions"`
}

// DeletedMark says a mark was deleted, so a client that still holds it stops.
type DeletedMark struct {
	ID         string    `json:"id"`
	BookFileID string    `json:"bookFileId"`
	DeletedAt  time.Time `json:"deletedAt"`
}

// SyncPosition is where a book was left, in both senses.
type SyncPosition struct {
	BookFileID string `json:"bookFileId"`
	// Position is where the reader is, which moves in both directions.
	Position string `json:"position"`
	// Furthest is how far they have got, which only ever advances.
	Furthest        string    `json:"furthest"`
	Percent         *float64  `json:"percent,omitempty"`
	FurthestPercent *float64  `json:"furthestPercent,omitempty"`
	ReadAt          time.Time `json:"readAt"`
	UpdatedAt       time.Time `json:"updatedAt"`
}

// SyncPush is what a client did while it was away.
type SyncPush struct {
	Annotations []PushedMark     `json:"annotations,omitempty"`
	Positions   []PushedPosition `json:"positions,omitempty"`
}

// PushedMark is one mark a client made, changed, or deleted offline.
type PushedMark struct {
	// ID is generated by the CLIENT, before any server has seen the mark. It
	// is what makes pushing the same change twice harmless, which matters
	// because the connection that drops is the one carrying the response.
	ID           string `json:"id"`
	BookFileID   string `json:"bookFileId"`
	Type         string `json:"type,omitempty"`
	Start        string `json:"start,omitempty"`
	End          string `json:"end,omitempty"`
	SelectedText string `json:"selectedText,omitempty"`
	Note         string `json:"note,omitempty"`
	Colour       string `json:"colour,omitempty"`
	// UpdatedAt is when the CLIENT made this change, and decides which version
	// wins against the server's. Not when it was sent: a phone that was in a
	// tunnel for a week did not edit anything at the moment it found signal.
	UpdatedAt time.Time `json:"updatedAt"`
	// Deleted says this change is a deletion, which carries nothing else.
	Deleted bool `json:"deleted,omitempty"`
}

// PushedPosition is where a client says a book was left.
type PushedPosition struct {
	BookFileID string   `json:"bookFileId"`
	Position   string   `json:"position"`
	Percent    *float64 `json:"percent,omitempty"`
	// ReadAt is when the reader was at this position, by the client's clock.
	ReadAt time.Time `json:"readAt"`
}

// SyncPushResult says what became of each pushed change.
type SyncPushResult struct {
	Marks []MarkResult `json:"marks"`
}

// MarkResult is the fate of one pushed mark.
type MarkResult struct {
	ID string `json:"id"`
	// Status is "applied", "stale" - the server held a newer version, which
	// arrives on the next pull - or "refused", for a book this person cannot
	// open.
	Status string `json:"status"`
}

// AccountDeleted reports what deleting an account did to the books it owned.
type AccountDeleted struct {
	// BooksArchived is how many books were kept in the archive: against the
	// deleted name, or - when erasing - with no name, because somebody else's
	// upload of the same bytes is still waiting to make them theirs.
	BooksArchived int `json:"booksArchived"`
	// FilesMoved is how many of those files Lexarii actually moved into its
	// archive. The rest live in a library directory, which is read-only and is
	// not Lexarii's to tidy: they stay exactly where their owner put them.
	FilesMoved int `json:"filesMoved"`
	// FilesErased is how many books only this person held were erased
	// rather than archived (LEXARII_DELETED_ACCOUNT_FILES=erase): their
	// records, what was derived from them, and the copies Lexarii stored.
	// Zero when archiving.
	FilesErased int `json:"filesErased"`
	// CopiesPending counts the content hashes - of erased books, and of the
	// person's own uploads that were still waiting to be extracted - whose
	// stored copies could not be deleted yet: the disk refused, or an upload
	// of the same bytes is on its way. They are owed, and deleted later
	// without anybody asking. Zero when archiving.
	CopiesPending int `json:"copiesPending"`
}

// FilesErased reports what erasing a person's books did.
type FilesErased struct {
	// FilesErased is how many books only this person held were erased: their
	// records, what was derived from them, and the copies Lexarii stored.
	FilesErased int `json:"filesErased"`
	// CopiesDeleted is how many stored copies were deleted from disk. A book
	// in the library directory has none: that directory is read-only and
	// never touched.
	CopiesDeleted int `json:"copiesDeleted"`
	// CopiesPending counts the content hashes of erased books whose stored
	// copies could not be deleted yet - the disk refused, or an upload of
	// the same bytes is on its way. They are deleted later without anybody
	// asking.
	CopiesPending int `json:"copiesPending"`
	// Kept counts books only this person holds that somebody else's upload
	// of the same bytes is still waiting for, which were left alone.
	Kept int `json:"kept"`
	// Withdrawn counts this person's own uploads that were not extracted yet
	// and were withdrawn: no longer waiting to reach their shelf, a queued
	// extraction cancelled, the stored copy deleted or owed.
	Withdrawn int `json:"withdrawn"`
}

// ArchivedBook is a book whose owner deleted their account.
type ArchivedBook struct {
	BookFileID string `json:"bookFileId"`
	Title      string `json:"title"`
	Format     string `json:"format"`
	// LeftBy is the name of the account that owned it, kept as text because the
	// account it would point at no longer exists.
	LeftBy     string    `json:"leftBy"`
	ArchivedAt time.Time `json:"archivedAt"`
	// Kept is true when the file was moved into Lexarii's archive. False
	// means it is still in the library directory it was scanned from.
	Kept bool `json:"kept"`
}

// ArchivedBooks is everything waiting to be claimed.
type ArchivedBooks struct {
	Items []ArchivedBook `json:"items"`
}

// ClaimRequest hands an archived book to somebody. An empty owner means the
// administrator making the request.
type ClaimRequest struct {
	OwnerID string `json:"ownerId,omitempty"`
}

// RemoveForEveryoneRequest takes a book file off the installation for
// everybody and blocks its bytes from coming back.
type RemoveForEveryoneRequest struct {
	// Reason is recorded on the blocklist, for administrators. Required; at
	// most 1000 characters (and 4000 bytes).
	Reason string `json:"reason"`
	// PurgeQuotes also erases the file's words kept outside it: the text each
	// highlight on it quotes, and the excerpt each chat citation of it keeps.
	// The highlights, notes and citations themselves stay. Default false.
	PurgeQuotes bool `json:"purgeQuotes,omitempty"`
}

// RemovedForEveryone reports how many people's shelves the file left: its
// holders, and anybody whose upload of it was still waiting to be extracted.
// A count, not a list: who held a file is each of them's own business.
type RemovedForEveryone struct {
	RemovedFor int `json:"removedFor"`
}

// BlockedFile is one entry on the blocklist: bytes an administrator removed
// from the installation, refused if anybody adds them again.
type BlockedFile struct {
	ContentHash string    `json:"contentHash"`
	Reason      string    `json:"reason"`
	BlockedAt   time.Time `json:"blockedAt"`
	// BlockedBy is the administrator who removed it, or absent once their
	// account is gone. The block outlives them.
	BlockedBy string `json:"blockedBy,omitempty"`
	// Title is the title the file had, while its record lasts. Absent once
	// the record has expired with the archive, when only the hash is left.
	Title string `json:"title,omitempty"`
	// CopiesRemain is true while some copy Lexarii itself stored of the file
	// may still be on disk: deleting them failed, or has not run yet. Finish
	// with POST /admin/blocked/{hash}/discard. Never about the library
	// directory, which is not deleted from.
	CopiesRemain bool `json:"copiesRemain"`
}

// DiscardedCopies reports whether, after another attempt, any copy Lexarii
// stored of a removed file may still be on disk.
type DiscardedCopies struct {
	CopiesRemain bool `json:"copiesRemain"`
}

// BlockedFiles is the blocklist, most recent first.
type BlockedFiles struct {
	Items []BlockedFile `json:"items"`
}

// NoteShareList is who someone shares their own marks on a book with.
//
// Items are bare account ids, which is all a client listing everybody from
// /people needs. People names them too, for a client on an installation that
// does not list its people. Neither says anything about who else can read the
// book.
type NoteShareList struct {
	Items  []string    `json:"items"`
	People []Recipient `json:"people"`
}

// SharingActivity is what has happened lately to the books this person lent.
type SharingActivity struct {
	Items []ShareActivity `json:"items"`
	// Notable counts the items worth acting on, so a client can decide whether
	// to say anything without re-deriving the rule.
	Notable int `json:"notable"`
	// Since is the start of the window these items cover.
	Since time.Time `json:"since"`
}

// CoverTarget is a file whose cover has not been rendered yet.
//
// A PDF's cover is its first page, and rasterising that needs a renderer the
// server does not have: the client does it with pdf.js and uploads the result.
type CoverTarget struct {
	BookFileID string `json:"bookFileId"`
	Format     string `json:"format"`
}

// CoverTargets is the work queue for cover rendering.
type CoverTargets struct {
	Items []CoverTarget `json:"items"`
	// Remaining is how many books still have no cover, across the whole
	// library and not just this batch. The client works through 24 at a time,
	// so counting what it was handed would report the same number on every
	// round and read as though nothing were happening.
	Remaining int `json:"remaining"`
}
