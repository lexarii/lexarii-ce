-- SPDX-License-Identifier: AGPL-3.0-or-later

-- Splits author rows that credit several people in one name.
--
-- Publisher metadata routinely puts a whole writing team in one <dc:creator>
-- element, and until now ingest recorded that string verbatim. The result is an
-- author who does not exist: measured on a real 169-book library, 39 of 103
-- author rows were composites, including "Kevlin Henney and Trisha Gee" with
-- two books to their joint name and neither person present on their own.
--
-- Ingest now splits these on the way in (internal/ingest.SplitContributors).
-- This migration applies the same rule to rows already stored. It is a data
-- repair and cannot be undone: once "A and B" has been replaced by A and B, the
-- original string is gone. Down is therefore a no-op rather than a lie.
--
-- The rule, matching the Go implementation:
--   * "/", ";", "&" and the WORD "and" separate people.
--   * A comma separates people too, EXCEPT when the string is one person in
--     sort-name form - "Costa, Peter", "de Graaf, Robert". That is recognised
--     by a single comma preceded by a surname: one token, optionally behind a
--     run of particles.
--   * A degree or honorific standing alone is not a person and is dropped.

-- +goose Up

-- +goose StatementBegin
CREATE FUNCTION lexarii_split_contributors(raw text) RETURNS text[] AS $fn$
DECLARE
    parts     text[];
    expanded  text[] := '{}';
    result    text[] := '{}';
    part      text;
    piece     text;
    surname   text[];
    honorific text[] := ARRAY['phd','ph.d','md','m.d','msc','m.sc','mba','bsc','b.sc',
                              'ma','m.a','ba','b.a','jr','sr','ii','iii','iv','esq',
                              'dr','prof','pe','p.e','cissp'];
    particle  text[] := ARRAY['de','del','della','der','den','di','da','dos','du','la',
                              'le','van','von','ter','ten','bin','ibn','al','st','mac',
                              'mc','o'''];
BEGIN
    parts := ARRAY(
        SELECT btrim(p)
        FROM regexp_split_to_table(raw, '\s*(?:/|;|&|\yand\y)\s*', 'i') AS p
        WHERE btrim(p) <> ''
    );

    IF coalesce(array_length(parts, 1), 0) = 0 THEN
        RETURN ARRAY[raw];
    END IF;

    IF array_length(parts, 1) > 1 THEN
        -- A conjunction proves the string is a list, so its commas separate
        -- people as well.
        FOREACH part IN ARRAY parts LOOP
            expanded := expanded || string_to_array(part, ',');
        END LOOP;
    ELSE
        piece := parts[1];
        IF position(',' in piece) = 0 THEN
            expanded := ARRAY[piece];
        ELSIF (length(piece) - length(replace(piece, ',', ''))) > 1 THEN
            -- More than one comma is a list; a sort name has exactly one.
            expanded := string_to_array(piece, ',');
        ELSE
            surname := regexp_split_to_array(btrim(split_part(piece, ',', 1)), '\s+');
            WHILE array_length(surname, 1) > 1
                  AND lower(rtrim(surname[1], '.')) = ANY(particle) LOOP
                surname := surname[2:];
            END LOOP;
            IF array_length(surname, 1) = 1 THEN
                expanded := ARRAY[piece];  -- Sort-name form: one person.
            ELSE
                expanded := string_to_array(piece, ',');
            END IF;
        END IF;
    END IF;

    FOREACH piece IN ARRAY expanded LOOP
        piece := btrim(btrim(piece), ',');
        CONTINUE WHEN piece = '';
        CONTINUE WHEN lower(replace(rtrim(piece, '.'), ' ', '')) = ANY(honorific);
        result := result || piece;
    END LOOP;

    -- Never destroy a name by splitting it into nothing.
    IF coalesce(array_length(result, 1), 0) = 0 THEN
        RETURN ARRAY[raw];
    END IF;
    RETURN result;
END;
$fn$ LANGUAGE plpgsql IMMUTABLE;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE FUNCTION lexarii_sort_name(name text) RETURNS text AS $fn$
DECLARE
    parts    text[];
    suffix   text := '';
    last     int;
    first    int;
    suffixes text[] := ARRAY['jr','jr.','sr','sr.','i','ii','iii','iv'];
    particle text[] := ARRAY['van','von','de','del','della','di','da','du','la','le',
                             'den','der','ten','ter','bin','ibn'];
BEGIN
    name := btrim(regexp_replace(name, '\s+', ' ', 'g'));
    IF name = '' OR position(',' in name) > 0 THEN
        RETURN name;  -- Already "Surname, Forenames", or nothing to do.
    END IF;

    parts := string_to_array(name, ' ');
    last := array_length(parts, 1);
    IF last = 1 THEN
        RETURN name;
    END IF;

    IF last > 2 AND lower(parts[last]) = ANY(suffixes) THEN
        suffix := ' ' || parts[last];
        last := last - 1;
    END IF;

    -- Nobiliary particles travel with the surname: "van Gogh", "Le Guin".
    first := last;
    WHILE first > 2 AND lower(parts[first - 1]) = ANY(particle) LOOP
        first := first - 1;
    END LOOP;

    RETURN array_to_string(parts[first:last], ' ') || suffix
           || ', ' || array_to_string(parts[1:first - 1], ' ');
END;
$fn$ LANGUAGE plpgsql IMMUTABLE;
-- +goose StatementEnd

-- +goose StatementBegin
DO $mig$
DECLARE
    composite RECORD;
    names     text[];
    nm        text;
    target    uuid;
    pos       int;
    split     int := 0;
BEGIN
    FOR composite IN SELECT id, name FROM authors ORDER BY name LOOP
        names := lexarii_split_contributors(composite.name);
        CONTINUE WHEN array_length(names, 1) = 1 AND names[1] = composite.name;

        pos := 0;
        FOREACH nm IN ARRAY names LOOP
            -- Reuse the person's existing row when they are already known
            -- under their own name, which is the point of the exercise.
            INSERT INTO authors (name, sort_name)
            VALUES (nm, lexarii_sort_name(nm))
            ON CONFLICT (lower(name)) DO UPDATE SET name = authors.name
            RETURNING id INTO target;

            INSERT INTO work_authors (work_id, author_id, role, position)
            SELECT wa.work_id, target, wa.role, wa.position + pos
            FROM work_authors wa
            WHERE wa.author_id = composite.id
            ON CONFLICT (work_id, author_id, role) DO NOTHING;

            pos := pos + 1;
        END LOOP;

        DELETE FROM work_authors WHERE author_id = composite.id;
        DELETE FROM authors WHERE id = composite.id;
        split := split + 1;
    END LOOP;

    RAISE NOTICE 'split % composite author rows', split;
END;
$mig$;
-- +goose StatementEnd

DROP FUNCTION lexarii_split_contributors(text);
DROP FUNCTION lexarii_sort_name(text);

-- +goose Down

-- Deliberately empty. The composite strings were replaced by the people they
-- named and are not recoverable; pretending otherwise would be worse than
-- saying so. Re-ingesting the library restores whatever the files declare.
SELECT 1;
