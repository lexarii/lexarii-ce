-- +goose Up

-- Authentication is pluggable from the start.
--
-- A user is a person; an IDENTITY is one way that person proves who they are.
-- Separating them is the whole point: the same person may sign in with a local
-- password today, through OIDC at work tomorrow, and from an LDAP directory
-- after a migration, and all three must land on the same library, the same
-- reading positions, and the same annotations.
--
-- Bolting a second provider onto a users table that owns a password column is
-- the painful version of this, and it is painful precisely because the rows
-- already exist by then.
CREATE TABLE user_identities (
    id      uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,

    -- Provider is TEXT, not an enum. Adding OIDC, LDAP, or a proxy header
    -- provider must be a configuration change, not a schema migration - an enum
    -- would make every new integration a database deployment.
    provider text NOT NULL,

    -- The provider's own stable identifier for this person: a username for
    -- local, `sub` for OIDC, the DN or objectGUID for LDAP/AD.
    --
    -- For OIDC this must be `sub` and never the email address: emails get
    -- reassigned between employees, and keying on one hands a new joiner the
    -- previous holder's library.
    provider_user_id text NOT NULL,

    -- Secret holds a password hash for the local provider and nothing at all
    -- for federated ones, where the provider holds the credential.
    secret text,

    -- What the provider told us about this person at last sign-in. Kept for
    -- display and for matching, never treated as authoritative.
    email        text,
    display_name text,

    -- Raw claims or attributes, for debugging a federation that is not behaving.
    provider_data jsonb NOT NULL DEFAULT '{}',

    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now(),
    last_login_at timestamptz,

    CONSTRAINT user_identities_provider_not_blank CHECK (length(trim(provider)) > 0),
    CONSTRAINT user_identities_subject_not_blank CHECK (length(trim(provider_user_id)) > 0),
    -- Only the local provider stores a credential. A federated identity holding
    -- a secret means something has been copied that should not have been.
    CONSTRAINT user_identities_secret_only_local
        CHECK (secret IS NULL OR provider = 'local')
);

-- One account per subject per provider.
CREATE UNIQUE INDEX user_identities_provider_subject_key
    ON user_identities (provider, lower(provider_user_id));

CREATE INDEX user_identities_user_id_idx ON user_identities (user_id);
CREATE INDEX user_identities_email_idx ON user_identities (lower(email)) WHERE email IS NOT NULL;

CREATE TRIGGER user_identities_set_updated_at
    BEFORE UPDATE ON user_identities
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- Move any existing password onto a local identity, then drop it from users.
-- After this the users table knows nothing about how anyone authenticates,
-- which is what lets a new provider be added without touching it.
-- WHERE NOT EXISTS, because this may run a second time.
--
-- `make migrate-reset` is a supported target, so down-then-up is a path
-- somebody takes deliberately. Unconditional, this manufactured a second
-- local identity for every user on the way back up - and for anyone who signs
-- in through a directory or an IdP, a local identity with a NULL secret is an
-- account that cannot be signed into by any route.
INSERT INTO user_identities (user_id, provider, provider_user_id, secret, email)
SELECT u.id, 'local', u.username, NULLIF(u.password_hash, ''), u.email
FROM users u
WHERE NOT EXISTS (
    SELECT 1 FROM user_identities ui
     WHERE ui.user_id = u.id AND ui.provider = 'local'
);

ALTER TABLE users DROP COLUMN password_hash;

-- Sessions are provider-agnostic: once a person is authenticated it no longer
-- matters how. These columns are for showing someone their active sessions and
-- letting them revoke one.
ALTER TABLE sessions
    ADD COLUMN identity_id uuid REFERENCES user_identities (id) ON DELETE SET NULL,
    ADD COLUMN last_used_at timestamptz NOT NULL DEFAULT now(),
    ADD COLUMN revoked_at timestamptz,
    ADD COLUMN client_ip inet;

CREATE INDEX sessions_active_idx ON sessions (user_id, expires_at)
    WHERE revoked_at IS NULL;

-- +goose Down
DROP INDEX sessions_active_idx;
ALTER TABLE sessions
    DROP COLUMN client_ip,
    DROP COLUMN revoked_at,
    DROP COLUMN last_used_at,
    DROP COLUMN identity_id;

-- Refuse rather than quietly destroy a federated account.
--
-- users.password_hash can hold a local password and nothing else. Rolling back
-- past this point discards every OIDC subject, every LDAP binding and the
-- provider each account used - and the user cannot tell, because they still
-- appear in the list. Signing in through the IdP afterwards matches nothing
-- and creates a NEW account, leaving their library, annotations and reading
-- positions attached to a row nobody can reach.
--
-- There is no honest way to represent those identities in the old shape, so
-- this stops and says so instead of pretending.
-- +goose StatementBegin
DO $$
DECLARE federated integer;
BEGIN
    SELECT count(*) INTO federated FROM user_identities WHERE provider <> 'local';
    IF federated > 0 THEN
        RAISE EXCEPTION USING
            MESSAGE = format('cannot roll back past identities: %s federated identities would be destroyed', federated),
            HINT = 'The schema before this migration can only hold a local password. '
                   'Those accounts would lose the provider mapping that identifies them, '
                   'and signing in again would create new accounts orphaning their libraries. '
                   'Remove or migrate them deliberately first.';
    END IF;
END
$$;
-- +goose StatementEnd

ALTER TABLE users ADD COLUMN password_hash text NOT NULL DEFAULT '';
UPDATE users SET password_hash = COALESCE(
    (SELECT ui.secret FROM user_identities ui
     WHERE ui.user_id = users.id AND ui.provider = 'local' LIMIT 1), '');
ALTER TABLE users ALTER COLUMN password_hash DROP DEFAULT;

DROP TABLE user_identities;
