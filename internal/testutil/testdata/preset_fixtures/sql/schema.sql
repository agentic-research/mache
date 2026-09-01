CREATE TABLE users (
    id          BIGSERIAL PRIMARY KEY,
    email       TEXT UNIQUE NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE sessions (
    id          UUID PRIMARY KEY,
    user_id     BIGINT NOT NULL REFERENCES users(id),
    expires_at  TIMESTAMPTZ NOT NULL
);

CREATE INDEX idx_sessions_user_id ON sessions (user_id);
CREATE INDEX idx_sessions_expires_at ON sessions (expires_at);

CREATE VIEW active_sessions AS
SELECT s.id, s.user_id, u.email, s.expires_at
FROM sessions s
JOIN users u ON u.id = s.user_id
WHERE s.expires_at > now();

CREATE TYPE entry_kind AS ENUM ('file', 'directory', 'symlink');

CREATE FUNCTION user_count() RETURNS BIGINT
LANGUAGE SQL
AS $$
    SELECT COUNT(*) FROM users
$$;

CREATE TRIGGER touch_user_updated
BEFORE UPDATE ON users
FOR EACH ROW
EXECUTE FUNCTION trigger_set_timestamp();
