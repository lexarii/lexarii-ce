-- +goose Up

CREATE TYPE message_role AS ENUM ('user', 'assistant', 'system');

-- The router classifies every question before retrieving anything. This is what
-- keeps a local model's context window survivable.
CREATE TYPE query_intent AS ENUM (
    'factual-lookup',
    'find-book',
    'summarise-book',
    'broad-topic',
    'metadata'
);

CREATE TABLE conversations (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id       uuid        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    title         text,
    -- Optional scope: a conversation may be pinned to a collection or to a
    -- specific book, which narrows every retrieval in the thread.
    collection_id uuid REFERENCES collections (id) ON DELETE SET NULL,
    book_file_id  uuid REFERENCES book_files (id) ON DELETE SET NULL,
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX conversations_user_recent_idx ON conversations (user_id, updated_at DESC);

CREATE TRIGGER conversations_set_updated_at
    BEFORE UPDATE ON conversations
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

CREATE TABLE messages (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    conversation_id uuid         NOT NULL REFERENCES conversations (id) ON DELETE CASCADE,
    seq             int          NOT NULL,
    role            message_role NOT NULL,
    content         text         NOT NULL,

    -- Assistant turns record how the answer was produced, so a disappointing
    -- answer can be diagnosed rather than guessed at.
    intent          query_intent,
    model_id        text,
    prompt_tokens   int,
    completion_tokens int,
    retrieval_ms    int,
    generation_ms   int,
    -- True when retrieval scores were too low to support an answer and the
    -- model was told to say so. Confabulation is a bug; this is how it is caught.
    unsupported     boolean      NOT NULL DEFAULT false,

    created_at      timestamptz  NOT NULL DEFAULT now(),

    CONSTRAINT messages_intent_only_on_assistant CHECK (intent IS NULL OR role = 'assistant'),
    CONSTRAINT messages_seq_non_negative CHECK (seq >= 0)
);

CREATE UNIQUE INDEX messages_conversation_seq_key ON messages (conversation_id, seq);

-- Citations are the trust mechanism of the whole feature: an assistant claim
-- without one is a defect. Linking to a chunk links transitively to a locator,
-- which is what makes "jump to this passage" work.
CREATE TABLE citations (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    message_id uuid        NOT NULL REFERENCES messages (id) ON DELETE CASCADE,
    chunk_id   uuid        NOT NULL REFERENCES chunks (id) ON DELETE CASCADE,
    position   int         NOT NULL DEFAULT 0, -- order shown in the answer
    -- Rank and score at retrieval time, kept for evaluation.
    rank       int,
    score      real,
    created_at timestamptz NOT NULL DEFAULT now(),

    UNIQUE (message_id, chunk_id)
);

CREATE INDEX citations_message_idx ON citations (message_id, position);
CREATE INDEX citations_chunk_idx ON citations (chunk_id);

-- +goose Down
DROP TABLE citations;
DROP TABLE messages;
DROP TABLE conversations;
DROP TYPE query_intent;
DROP TYPE message_role;
