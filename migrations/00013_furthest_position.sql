-- +goose Up

-- A reading position and how far someone has read are two different facts.
--
-- Until now there was one, resolved to whichever was FURTHEST. That protects
-- the case it was written for - a phone read ahead, a laptop must not rewind it
-- - but it makes deliberate navigation impossible to save: open the contents,
-- jump back to chapter 7 to check something, close the book, and it reopens at
-- chapter 69, because the furthest position won. Every save of an earlier
-- position was silently discarded.
--
-- So they are separated. The CURRENT position is where the reader actually is
-- and takes the most recent write. The FURTHEST position only ever advances and
-- is what "62% read" means on a shelf. Neither can now overwrite the other.

ALTER TABLE reading_progress
    ADD COLUMN furthest_major   integer,
    ADD COLUMN furthest_path    text,
    ADD COLUMN furthest_offset  integer,
    ADD COLUMN furthest_percent real;

-- Every existing row was written under the furthest-wins rule, so what is
-- stored IS the furthest position. It is also the best available guess at the
-- current one, which is why the existing columns are left alone.
UPDATE reading_progress
SET furthest_major   = major,
    furthest_path    = path,
    furthest_offset  = "offset",
    furthest_percent = percent;

ALTER TABLE reading_progress
    ALTER COLUMN furthest_major SET NOT NULL,
    ALTER COLUMN furthest_path SET NOT NULL,
    ALTER COLUMN furthest_path SET DEFAULT '',
    ALTER COLUMN furthest_offset SET NOT NULL,
    ALTER COLUMN furthest_offset SET DEFAULT 0,
    ADD CONSTRAINT reading_progress_furthest_major_non_negative
        CHECK (furthest_major >= 0),
    ADD CONSTRAINT reading_progress_furthest_offset_non_negative
        CHECK (furthest_offset >= 0),
    ADD CONSTRAINT reading_progress_furthest_percent_range
        CHECK (furthest_percent IS NULL OR (furthest_percent >= 0 AND furthest_percent <= 1));

-- +goose Down

ALTER TABLE reading_progress
    DROP CONSTRAINT reading_progress_furthest_major_non_negative,
    DROP CONSTRAINT reading_progress_furthest_offset_non_negative,
    DROP CONSTRAINT reading_progress_furthest_percent_range,
    DROP COLUMN furthest_major,
    DROP COLUMN furthest_path,
    DROP COLUMN furthest_offset,
    DROP COLUMN furthest_percent;
