-- +goose Up

-- ---------------------------------------------------------------------------
-- Chunks: retrieval units carrying their own locators
-- ---------------------------------------------------------------------------

CREATE TABLE chunks (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    book_file_id uuid        NOT NULL REFERENCES book_files (id) ON DELETE CASCADE,
    -- Position of this chunk in reading order within the file. Cheap neighbour
    -- expansion: seq - 1 and seq + 1.
    seq          int         NOT NULL,

    start_major  int         NOT NULL,
    start_path   text        NOT NULL DEFAULT '',
    start_offset int         NOT NULL DEFAULT 0,
    end_major    int         NOT NULL,
    end_path     text        NOT NULL DEFAULT '',
    end_offset   int         NOT NULL DEFAULT 0,

    chapter_title text,
    content       text        NOT NULL,
    token_count   int         NOT NULL,
    -- Chunking is versioned: changing strategy means re-chunking, and the system
    -- must be able to tell old rows from new.
    strategy_version int      NOT NULL,

    created_at   timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT chunks_content_not_blank CHECK (length(trim(content)) > 0),
    CONSTRAINT chunks_token_count_positive CHECK (token_count > 0),
    CONSTRAINT chunks_range_ordered CHECK (
        (start_major, start_path, start_offset) <= (end_major, end_path, end_offset)
    ),
    CONSTRAINT chunks_seq_non_negative CHECK (seq >= 0)
);

CREATE UNIQUE INDEX chunks_file_seq_key ON chunks (book_file_id, strategy_version, seq);
CREATE INDEX chunks_file_position_idx ON chunks (book_file_id, start_major, start_path, start_offset);

-- Sparse half of hybrid retrieval. Computed in the database, never in Go: a
-- generated column cannot drift from the content the way application code can.
ALTER TABLE chunks
    ADD COLUMN tsv tsvector
    GENERATED ALWAYS AS (
        setweight(to_tsvector('english', coalesce(chapter_title, '')), 'B') ||
        setweight(to_tsvector('english', content), 'A')
    ) STORED;

CREATE INDEX chunks_tsv_idx ON chunks USING gin (tsv);

-- ---------------------------------------------------------------------------
-- Embeddings
--
-- DIMENSION IS FIXED AT 768 (nomic-embed-text-v1.5, bge-base, e5-base).
-- pgvector requires a fixed dimension to build an HNSW index, so a model with a
-- different width (bge-large is 1024, all-MiniLM is 384) needs a migration that
-- alters this column and rebuilds the index. model_id and dimensions are stored
-- per row so mismatched vectors are always identifiable rather than silently
-- mixed into a search, which returns nonsense rather than an error.
-- ---------------------------------------------------------------------------

CREATE TABLE embeddings (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    chunk_id   uuid        NOT NULL REFERENCES chunks (id) ON DELETE CASCADE,
    model_id   text        NOT NULL,
    dimensions int         NOT NULL,
    -- Normalised on write, so cosine distance and inner product agree.
    vector     vector(768) NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT embeddings_dimensions_match CHECK (dimensions = 768)
);

-- One vector per chunk per model: re-embedding with the same model is an upsert,
-- and switching models leaves the old rows intact and re-embeddable.
CREATE UNIQUE INDEX embeddings_chunk_model_key ON embeddings (chunk_id, model_id);
CREATE INDEX embeddings_model_idx ON embeddings (model_id);

-- Dense half of hybrid retrieval. Built here while the table is empty; on a
-- populated table this must be CREATE INDEX CONCURRENTLY after a bulk load,
-- never before it.
CREATE INDEX embeddings_vector_hnsw_idx ON embeddings
    USING hnsw (vector vector_cosine_ops)
    WITH (m = 16, ef_construction = 64);

-- ---------------------------------------------------------------------------
-- Work-level topic embeddings, for the find-book intent
--
-- "Which of my books covers category theory" is answered from work summaries,
-- not from passages. Separate table, same fixed dimension.
-- ---------------------------------------------------------------------------

CREATE TABLE work_embeddings (
    work_id    uuid        NOT NULL REFERENCES works (id) ON DELETE CASCADE,
    model_id   text        NOT NULL,
    vector     vector(768) NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),

    PRIMARY KEY (work_id, model_id)
);

CREATE INDEX work_embeddings_vector_hnsw_idx ON work_embeddings
    USING hnsw (vector vector_cosine_ops)
    WITH (m = 16, ef_construction = 64);

-- +goose Down
DROP TABLE work_embeddings;
DROP TABLE embeddings;
DROP TABLE chunks;
