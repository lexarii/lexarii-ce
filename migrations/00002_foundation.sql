-- +goose Up

-- Shared updated_at trigger. Every table with an updated_at column attaches this
-- rather than relying on the application to remember.
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION set_updated_at() RETURNS trigger AS $$
BEGIN
    NEW.updated_at = now();
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
-- +goose StatementEnd

CREATE TABLE users (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    username      text        NOT NULL,
    email         text,
    password_hash text        NOT NULL,
    is_admin      boolean     NOT NULL DEFAULT false,
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT users_username_not_blank CHECK (length(trim(username)) > 0)
);

-- Case-insensitive uniqueness: "Adam" and "adam" are the same account.
CREATE UNIQUE INDEX users_username_lower_key ON users (lower(username));
CREATE UNIQUE INDEX users_email_lower_key ON users (lower(email)) WHERE email IS NOT NULL;

CREATE TRIGGER users_set_updated_at
    BEFORE UPDATE ON users
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

CREATE TABLE sessions (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id    uuid        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    token_hash text        NOT NULL UNIQUE,
    user_agent text,
    expires_at timestamptz NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT sessions_expiry_in_future CHECK (expires_at > created_at)
);

CREATE INDEX sessions_user_id_idx ON sessions (user_id);
CREATE INDEX sessions_expires_at_idx ON sessions (expires_at);

-- +goose Down
DROP TABLE sessions;
DROP TABLE users;
DROP FUNCTION set_updated_at();
