-- +goose Up

CREATE TYPE annotation_type AS ENUM ('highlight', 'note', 'bookmark');

-- ---------------------------------------------------------------------------
-- Locator storage
--
-- A Locator is stored as three ordered components rather than as its canonical
-- string, because string ordering is not locator ordering and reading position,
-- chunk ordering, and citation ordering all need real comparisons:
--
--   major  -> spine index (EPUB) or page number (PDF)
--   path   -> element path within the spine document (EPUB), '' for PDF
--   offset -> character offset
--
-- ORDER BY (major, path, offset) matches domain.Locator.Compare exactly.
-- The canonical string form is reconstructed in Go, never stored.
-- ---------------------------------------------------------------------------

CREATE TABLE reading_progress (
    user_id        uuid        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    book_file_id   uuid        NOT NULL REFERENCES book_files (id) ON DELETE CASCADE,
    major          int         NOT NULL,
    path           text        NOT NULL DEFAULT '',
    "offset"       int         NOT NULL DEFAULT 0,
    -- Display only. The locator above is the truth; this is a rendering of it
    -- and must never be used to restore a position.
    percent        real,
    last_read_at   timestamptz NOT NULL DEFAULT now(),
    updated_at     timestamptz NOT NULL DEFAULT now(),

    PRIMARY KEY (user_id, book_file_id),
    CONSTRAINT reading_progress_major_non_negative CHECK (major >= 0),
    CONSTRAINT reading_progress_offset_non_negative CHECK ("offset" >= 0),
    CONSTRAINT reading_progress_percent_range CHECK (percent IS NULL OR (percent >= 0 AND percent <= 1))
);

CREATE INDEX reading_progress_recent_idx ON reading_progress (user_id, last_read_at DESC);

CREATE TRIGGER reading_progress_set_updated_at
    BEFORE UPDATE ON reading_progress
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

CREATE TABLE annotations (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id      uuid            NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    book_file_id uuid            NOT NULL REFERENCES book_files (id) ON DELETE CASCADE,
    type         annotation_type NOT NULL,

    start_major  int             NOT NULL,
    start_path   text            NOT NULL DEFAULT '',
    start_offset int             NOT NULL DEFAULT 0,
    end_major    int             NOT NULL,
    end_path     text            NOT NULL DEFAULT '',
    end_offset   int             NOT NULL DEFAULT 0,

    -- The text as it read when the annotation was made. If the anchor fails to
    -- resolve after a re-ingest the annotation becomes orphaned and is shown to
    -- the user with this text. A user's note is never silently dropped.
    selected_text text,
    note          text,
    colour        text,
    orphaned_at   timestamptz,

    created_at   timestamptz     NOT NULL DEFAULT now(),
    updated_at   timestamptz     NOT NULL DEFAULT now(),

    CONSTRAINT annotations_range_ordered CHECK (
        (start_major, start_path, start_offset) <= (end_major, end_path, end_offset)
    ),
    CONSTRAINT annotations_offsets_non_negative CHECK (start_offset >= 0 AND end_offset >= 0),
    CONSTRAINT annotations_majors_non_negative CHECK (start_major >= 0 AND end_major >= 0),
    CONSTRAINT annotations_note_has_body CHECK (type <> 'note' OR length(trim(coalesce(note, ''))) > 0),
    CONSTRAINT annotations_highlight_has_text CHECK (type <> 'highlight' OR selected_text IS NOT NULL)
);

CREATE INDEX annotations_book_position_idx
    ON annotations (book_file_id, user_id, start_major, start_path, start_offset);
CREATE INDEX annotations_user_recent_idx ON annotations (user_id, created_at DESC);
CREATE INDEX annotations_orphaned_idx ON annotations (user_id) WHERE orphaned_at IS NOT NULL;

CREATE TRIGGER annotations_set_updated_at
    BEFORE UPDATE ON annotations
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- +goose Down
DROP TABLE annotations;
DROP TABLE reading_progress;
DROP TYPE annotation_type;
