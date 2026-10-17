// SPDX-License-Identifier: AGPL-3.0-or-later

package domain

import (
	"time"

	"github.com/google/uuid"
)

// DefaultArchiveRetention is how long the books of a deleted account wait to be
// claimed before their catalogue records are deleted.
//
// Six months. Long enough that somebody who left and came back finds their
// library where they put it, short enough that an installation is not holding
// rows naming a person indefinitely on the chance that they return. The FILES
// are kept whatever this says - the retention is about the record, not the
// book.
const DefaultArchiveRetention = 183 * 24 * time.Hour

// MetadataSource records where a metadata value came from. The UI must always
// be able to answer "where did this come from?", and publisher metadata always
// beats a model's guess.
type MetadataSource string

const (
	SourceEmbedded MetadataSource = "embedded" // from the file itself: OPF, XMP, ComicInfo
	SourceUser     MetadataSource = "user"
	SourceLLM      MetadataSource = "llm"
	SourceFilename MetadataSource = "filename"
	SourceExternal MetadataSource = "external" // OpenLibrary, Crossref
	SourceUnknown  MetadataSource = "unknown"
)

// IngestStatus is the extraction state of a BookFile.
type IngestStatus string

const (
	IngestPending     IngestStatus = "pending"
	IngestExtracting  IngestStatus = "extracting"
	IngestReady       IngestStatus = "ready"
	IngestNeedsOCR    IngestStatus = "needs_ocr" // a scanned PDF with no text layer
	IngestFailed      IngestStatus = "failed"
	IngestUnsupported IngestStatus = "unsupported"
)

// Author is a person. Separate from Work so "Kernighan & Ritchie" is two
// browsable people rather than one string.
type Author struct {
	ID        uuid.UUID `json:"id"`
	Name      string    `json:"name"`
	SortName  string    `json:"sortName"` // "Ritchie, Dennis"
	Aliases   []string  `json:"aliases,omitempty"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// AuthorRole distinguishes contributors on a Work.
type AuthorRole string

const (
	RoleAuthor      AuthorRole = "author"
	RoleEditor      AuthorRole = "editor"
	RoleTranslator  AuthorRole = "translator"
	RoleIllustrator AuthorRole = "illustrator"
)

// WorkAuthor links a Work to an Author in a given role.
type WorkAuthor struct {
	Author   Author     `json:"author"`
	Role     AuthorRole `json:"role"`
	Position int        `json:"position"` // billing order on the cover
}

// Work is a book as an idea: "Gödel, Escher, Bach", regardless of edition or
// format. Two files of the same book are one Work and therefore one shelf item.
type Work struct {
	ID          uuid.UUID    `json:"id"`
	Title       string       `json:"title"`
	Subtitle    string       `json:"subtitle,omitempty"`
	SortTitle   string       `json:"sortTitle"`
	Series      string       `json:"series,omitempty"`
	SeriesIndex *float64     `json:"seriesIndex,omitempty"`
	Language    string       `json:"language,omitempty"` // BCP-47
	Description string       `json:"description,omitempty"`
	Summary     string       `json:"summary,omitempty"` // model-generated; see EnrichmentRecord
	Authors     []WorkAuthor `json:"authors,omitempty"`
	Tags        []Tag        `json:"tags,omitempty"`
	Editions    []Edition    `json:"editions,omitempty"`
	CreatedAt   time.Time    `json:"createdAt"`
	UpdatedAt   time.Time    `json:"updatedAt"`
}

// Tag is a flat label, frequently machine-generated. Distinct from Collection,
// which is hand-curated and hierarchical.
type Tag struct {
	ID   uuid.UUID `json:"id"`
	Name string    `json:"name"`
	// Slug is the normalised form used for deduplication: lowercased, trimmed,
	// spaces to hyphens. Without it a 5000-book library grows 5000 near-duplicate
	// tags: "ml", "ML", "machine-learning", "Machine Learning".
	Slug      string         `json:"slug"`
	IsSubject bool           `json:"isSubject"` // part of the controlled subject vocabulary
	ParentID  *uuid.UUID     `json:"parentId,omitempty"`
	Source    MetadataSource `json:"source,omitempty"`
	// Confidence is set only for machine-generated tags.
	Confidence *float32 `json:"confidence,omitempty"`
}

// Edition is a specific published version of a Work: a printing with an ISBN,
// a publisher, and a cover.
type Edition struct {
	ID          uuid.UUID  `json:"id"`
	WorkID      uuid.UUID  `json:"workId"`
	ISBN13      string     `json:"isbn13,omitempty"`
	ISBN10      string     `json:"isbn10,omitempty"`
	Publisher   string     `json:"publisher,omitempty"`
	PublishedOn *time.Time `json:"publishedOn,omitempty"`
	PageCount   *int       `json:"pageCount,omitempty"`
	Language    string     `json:"language,omitempty"`
	// CoverPath is relative to the data directory. Never the library directory:
	// Lexarii does not write there.
	CoverPath string     `json:"coverPath,omitempty"`
	Files     []BookFile `json:"files,omitempty"`
	CreatedAt time.Time  `json:"createdAt"`
	UpdatedAt time.Time  `json:"updatedAt"`
}

// BookFile is an actual file in the read-only library directory.
type BookFile struct {
	ID        uuid.UUID `json:"id"`
	EditionID uuid.UUID `json:"editionId"`
	Path      string    `json:"path"`
	// ContentHash is SHA-256 of the file bytes and is file identity. The same
	// hash is the same file: a moved or renamed file is recognised and updated,
	// never duplicated, and re-ingest is a no-op.
	ContentHash string       `json:"contentHash"`
	Format      Format       `json:"format"`
	SizeBytes   int64        `json:"sizeBytes"`
	Status      IngestStatus `json:"status"`
	IngestError string       `json:"ingestError,omitempty"`
	// UnitCount is spine items for EPUB, pages for PDF. Bounds locator validation.
	UnitCount   *int       `json:"unitCount,omitempty"`
	ExtractedAt *time.Time `json:"extractedAt,omitempty"`
	CreatedAt   time.Time  `json:"createdAt"`
	UpdatedAt   time.Time  `json:"updatedAt"`
}

// Readable reports whether the file has usable extracted text. CBZ and CBR are
// library citizens but not retrieval citizens, and a scanned PDF has nothing to
// chunk until it is OCRed.
func (b BookFile) Readable() bool {
	return b.Status == IngestReady && (b.Format == FormatEPUB || b.Format == FormatPDF)
}

// Collection is a hand-curated, nestable grouping: "Maths", "To read",
// "PhD sources". Distinct from Tag, which is flat and cheap.
type Collection struct {
	ID          uuid.UUID  `json:"id"`
	UserID      uuid.UUID  `json:"userId"`
	Name        string     `json:"name"`
	Description string     `json:"description,omitempty"`
	ParentID    *uuid.UUID `json:"parentId,omitempty"`
	Position    int        `json:"position"`
	CreatedAt   time.Time  `json:"createdAt"`
	UpdatedAt   time.Time  `json:"updatedAt"`
}

// EnrichmentRecord is the provenance of anything a model wrote. Values land
// here first; a suggestion never silently overwrites embedded metadata.
type EnrichmentRecord struct {
	ID            uuid.UUID      `json:"id"`
	WorkID        uuid.UUID      `json:"workId"`
	Field         string         `json:"field"` // "summary", "tags", "series", ...
	Value         []byte         `json:"value"` // JSON
	Source        MetadataSource `json:"source"`
	ModelID       string         `json:"modelId,omitempty"`
	PromptVersion string         `json:"promptVersion,omitempty"`
	Confidence    *float32       `json:"confidence,omitempty"`
	GeneratedAt   time.Time      `json:"generatedAt"`
	AcceptedAt    *time.Time     `json:"acceptedAt,omitempty"`
	RejectedAt    *time.Time     `json:"rejectedAt,omitempty"`
}

// Pending reports whether the record is still awaiting the user's decision.
func (e EnrichmentRecord) Pending() bool {
	return e.AcceptedAt == nil && e.RejectedAt == nil
}
