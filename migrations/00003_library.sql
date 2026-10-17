-- +goose Up

-- Provenance of a metadata value. Publisher metadata always beats a model's
-- guess, and the UI must be able to answer "where did this come from?".
CREATE TYPE metadata_source AS ENUM ('embedded', 'user', 'llm', 'filename', 'external', 'unknown');

CREATE TYPE book_format AS ENUM ('epub', 'pdf', 'cbz', 'cbr', 'mobi', 'azw3');

CREATE TYPE ingest_status AS ENUM ('pending', 'extracting', 'ready', 'needs_ocr', 'failed', 'unsupported');

-- ---------------------------------------------------------------------------
-- Authors
-- ---------------------------------------------------------------------------

-- A person, not a string. "Kernighan & Ritchie" must be two browsable people.
CREATE TABLE authors (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    name       text        NOT NULL,
    sort_name  text        NOT NULL, -- "Ritchie, Dennis"
    aliases    text[]      NOT NULL DEFAULT '{}',
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT authors_name_not_blank CHECK (length(trim(name)) > 0)
);

CREATE UNIQUE INDEX authors_name_lower_key ON authors (lower(name));
-- Fuzzy author search is a different problem from chunk retrieval and gets its
-- own index.
CREATE INDEX authors_name_trgm_idx ON authors USING gin (name gin_trgm_ops);

CREATE TRIGGER authors_set_updated_at
    BEFORE UPDATE ON authors
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- ---------------------------------------------------------------------------
-- Works: a book as an idea, independent of edition or format
-- ---------------------------------------------------------------------------

CREATE TABLE works (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    title        text        NOT NULL,
    subtitle     text,
    sort_title   text        NOT NULL,
    series       text,
    series_index numeric(8, 2),
    language     text, -- BCP-47
    description  text,
    summary      text, -- model-generated; see enrichment_records for provenance
    created_at   timestamptz NOT NULL DEFAULT now(),
    updated_at   timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT works_title_not_blank CHECK (length(trim(title)) > 0),
    CONSTRAINT works_series_index_needs_series CHECK (series_index IS NULL OR series IS NOT NULL)
);

CREATE INDEX works_sort_title_idx ON works (sort_title);
CREATE INDEX works_title_trgm_idx ON works USING gin (title gin_trgm_ops);
CREATE INDEX works_series_idx ON works (series, series_index) WHERE series IS NOT NULL;

CREATE TRIGGER works_set_updated_at
    BEFORE UPDATE ON works
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

CREATE TABLE work_authors (
    work_id   uuid NOT NULL REFERENCES works (id) ON DELETE CASCADE,
    author_id uuid NOT NULL REFERENCES authors (id) ON DELETE CASCADE,
    role      text NOT NULL DEFAULT 'author', -- author, editor, translator, illustrator
    position  int  NOT NULL DEFAULT 0,        -- billing order on the cover

    PRIMARY KEY (work_id, author_id, role)
);

CREATE INDEX work_authors_author_id_idx ON work_authors (author_id);

-- ---------------------------------------------------------------------------
-- Tags: flat, numerous, frequently machine-generated
-- ---------------------------------------------------------------------------

CREATE TABLE tags (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    name          text        NOT NULL,
    -- Normalised form used for deduplication: lowercased, trimmed, spaces to
    -- hyphens. Without this a 5000-book library grows 5000 near-duplicate tags.
    slug          text        NOT NULL UNIQUE,
    is_subject    boolean     NOT NULL DEFAULT false, -- part of the controlled subject vocabulary
    parent_tag_id uuid REFERENCES tags (id) ON DELETE SET NULL,
    created_at    timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT tags_name_not_blank CHECK (length(trim(name)) > 0),
    CONSTRAINT tags_not_own_parent CHECK (parent_tag_id IS NULL OR parent_tag_id <> id)
);

CREATE INDEX tags_parent_idx ON tags (parent_tag_id) WHERE parent_tag_id IS NOT NULL;
CREATE INDEX tags_subject_idx ON tags (is_subject) WHERE is_subject;

CREATE TABLE work_tags (
    work_id    uuid            NOT NULL REFERENCES works (id) ON DELETE CASCADE,
    tag_id     uuid            NOT NULL REFERENCES tags (id) ON DELETE CASCADE,
    source     metadata_source NOT NULL DEFAULT 'user',
    confidence real,
    created_at timestamptz     NOT NULL DEFAULT now(),

    PRIMARY KEY (work_id, tag_id),
    CONSTRAINT work_tags_confidence_range CHECK (confidence IS NULL OR (confidence >= 0 AND confidence <= 1))
);

CREATE INDEX work_tags_tag_id_idx ON work_tags (tag_id);

-- ---------------------------------------------------------------------------
-- Editions: a specific published version of a Work
-- ---------------------------------------------------------------------------

CREATE TABLE editions (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    work_id      uuid        NOT NULL REFERENCES works (id) ON DELETE CASCADE,
    isbn13       text,
    isbn10       text,
    publisher    text,
    published_on date,
    page_count   int,
    language     text,
    cover_path   text, -- relative to the data directory, never the library directory
    created_at   timestamptz NOT NULL DEFAULT now(),
    updated_at   timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT editions_isbn13_shape CHECK (isbn13 IS NULL OR isbn13 ~ '^[0-9]{13}$'),
    CONSTRAINT editions_isbn10_shape CHECK (isbn10 IS NULL OR isbn10 ~ '^[0-9]{9}[0-9X]$'),
    CONSTRAINT editions_page_count_positive CHECK (page_count IS NULL OR page_count > 0)
);

CREATE INDEX editions_work_id_idx ON editions (work_id);
CREATE UNIQUE INDEX editions_isbn13_key ON editions (isbn13) WHERE isbn13 IS NOT NULL;

CREATE TRIGGER editions_set_updated_at
    BEFORE UPDATE ON editions
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- ---------------------------------------------------------------------------
-- BookFiles: actual files in the read-only library directory
-- ---------------------------------------------------------------------------

CREATE TABLE book_files (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    edition_id  uuid          NOT NULL REFERENCES editions (id) ON DELETE CASCADE,
    path        text          NOT NULL,
    -- SHA-256 of the file bytes. This is file identity: UNIQUE so re-ingesting
    -- is a no-op at the database level rather than in application logic, and so
    -- a moved or renamed file is recognised instead of duplicated.
    content_hash text         NOT NULL UNIQUE,
    format       book_format  NOT NULL,
    size_bytes   bigint       NOT NULL,
    status       ingest_status NOT NULL DEFAULT 'pending',
    ingest_error text,
    -- Number of spine items (EPUB) or pages (PDF); bounds locator validation.
    unit_count   int,
    extracted_at timestamptz,
    created_at   timestamptz  NOT NULL DEFAULT now(),
    updated_at   timestamptz  NOT NULL DEFAULT now(),

    CONSTRAINT book_files_hash_shape CHECK (content_hash ~ '^[0-9a-f]{64}$'),
    CONSTRAINT book_files_size_positive CHECK (size_bytes > 0),
    CONSTRAINT book_files_error_only_when_failed
        CHECK (ingest_error IS NULL OR status IN ('failed', 'needs_ocr', 'unsupported'))
);

CREATE INDEX book_files_edition_id_idx ON book_files (edition_id);
CREATE INDEX book_files_status_idx ON book_files (status);
CREATE UNIQUE INDEX book_files_path_key ON book_files (path);

CREATE TRIGGER book_files_set_updated_at
    BEFORE UPDATE ON book_files
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- ---------------------------------------------------------------------------
-- Collections: hand-curated and hierarchical, unlike tags
-- ---------------------------------------------------------------------------

CREATE TABLE collections (
    id                   uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id              uuid        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    name                 text        NOT NULL,
    description          text,
    parent_collection_id uuid REFERENCES collections (id) ON DELETE CASCADE,
    position             int         NOT NULL DEFAULT 0,
    created_at           timestamptz NOT NULL DEFAULT now(),
    updated_at           timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT collections_name_not_blank CHECK (length(trim(name)) > 0),
    CONSTRAINT collections_not_own_parent CHECK (parent_collection_id IS NULL OR parent_collection_id <> id)
);

CREATE INDEX collections_user_id_idx ON collections (user_id);
CREATE INDEX collections_parent_idx ON collections (parent_collection_id) WHERE parent_collection_id IS NOT NULL;

CREATE TRIGGER collections_set_updated_at
    BEFORE UPDATE ON collections
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

CREATE TABLE collection_works (
    collection_id uuid        NOT NULL REFERENCES collections (id) ON DELETE CASCADE,
    work_id       uuid        NOT NULL REFERENCES works (id) ON DELETE CASCADE,
    position      int         NOT NULL DEFAULT 0,
    added_at      timestamptz NOT NULL DEFAULT now(),

    PRIMARY KEY (collection_id, work_id)
);

CREATE INDEX collection_works_work_id_idx ON collection_works (work_id);

-- ---------------------------------------------------------------------------
-- Enrichment provenance
-- ---------------------------------------------------------------------------

CREATE TABLE enrichment_records (
    id             uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    work_id        uuid            NOT NULL REFERENCES works (id) ON DELETE CASCADE,
    field          text            NOT NULL, -- 'summary', 'tags', 'series', ...
    value          jsonb           NOT NULL,
    source         metadata_source NOT NULL,
    model_id       text,
    prompt_version text,
    confidence     real,
    generated_at   timestamptz     NOT NULL DEFAULT now(),
    -- NULL means "suggested, awaiting the user". A suggestion never silently
    -- overwrites embedded metadata.
    accepted_at    timestamptz,
    rejected_at    timestamptz,

    CONSTRAINT enrichment_confidence_range CHECK (confidence IS NULL OR (confidence >= 0 AND confidence <= 1)),
    CONSTRAINT enrichment_not_both_accepted_and_rejected CHECK (accepted_at IS NULL OR rejected_at IS NULL),
    CONSTRAINT enrichment_llm_has_model CHECK (source <> 'llm' OR model_id IS NOT NULL)
);

CREATE INDEX enrichment_work_field_idx ON enrichment_records (work_id, field);
CREATE INDEX enrichment_pending_idx ON enrichment_records (work_id)
    WHERE accepted_at IS NULL AND rejected_at IS NULL;

-- +goose Down
DROP TABLE enrichment_records;
DROP TABLE collection_works;
DROP TABLE collections;
DROP TABLE book_files;
DROP TABLE editions;
DROP TABLE work_tags;
DROP TABLE tags;
DROP TABLE work_authors;
DROP TABLE works;
DROP TABLE authors;
DROP TYPE ingest_status;
DROP TYPE book_format;
DROP TYPE metadata_source;
