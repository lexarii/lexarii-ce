-- +goose Up

-- A range must not run backwards. Element paths cannot say whether it does.
--
-- Both constraints compared (major, path, offset) as a row, which orders the
-- path as TEXT. Text order is not document order: in a chapter containing
-- `p[74]` followed by `<a id="x"><p>…</p></a>`, the second block's path is
-- `a[29]/p[4]`, which sorts BEFORE `p[74]` while appearing after it on the
-- page. A highlight or a chunk spanning those two blocks was rejected as
-- backwards when it was nothing of the kind.
--
-- This is the same lesson the locator columns were split for - a locator's
-- string form does not sort in reading order, which is why major, path, and
-- offset are separate columns rather than one text field. The CHECK quietly
-- reintroduced the assumption the columns exist to avoid.
--
-- So the constraint now asserts only what SQL can actually know:
--
--   * a later unit is later, always;
--   * within one unit and one element, a later offset is later;
--   * within one unit but ACROSS elements, nothing - deciding that needs the
--     document, which the database does not have.
--
-- Narrower, and true. The alternative is a constraint that rejects valid data,
-- which is worse than one that admits some invalid data: the reader already
-- treats a range it cannot resolve as an orphan.

ALTER TABLE annotations DROP CONSTRAINT annotations_range_ordered;
ALTER TABLE annotations ADD CONSTRAINT annotations_range_ordered CHECK (
    start_major < end_major
    OR (start_major = end_major AND (start_path <> end_path OR start_offset <= end_offset))
);

ALTER TABLE chunks DROP CONSTRAINT chunks_range_ordered;
ALTER TABLE chunks ADD CONSTRAINT chunks_range_ordered CHECK (
    start_major < end_major
    OR (start_major = end_major AND (start_path <> end_path OR start_offset <= end_offset))
);

-- +goose Down

ALTER TABLE annotations DROP CONSTRAINT annotations_range_ordered;
ALTER TABLE annotations ADD CONSTRAINT annotations_range_ordered CHECK (
    (start_major, start_path, start_offset) <= (end_major, end_path, end_offset)
);

ALTER TABLE chunks DROP CONSTRAINT chunks_range_ordered;
ALTER TABLE chunks ADD CONSTRAINT chunks_range_ordered CHECK (
    (start_major, start_path, start_offset) <= (end_major, end_path, end_offset)
);
