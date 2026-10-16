-- +goose Up
CREATE TABLE users (
    id            bigint      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    -- Stored lower-cased: the unique index is then case-insensitive.
    email         text        NOT NULL UNIQUE CHECK (email = lower(email) AND length(email) BETWEEN 3 AND 254),
    name          text        NOT NULL CHECK (length(name) BETWEEN 1 AND 100),
    password_hash bytea       NOT NULL, -- bcrypt
    created_at    timestamptz NOT NULL DEFAULT now()
);

-- Only a SHA-256 of the session token is stored: a leaked table does not
-- let anyone log in.
CREATE TABLE sessions (
    token_hash bytea       PRIMARY KEY,
    user_id    bigint      NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    created_at timestamptz NOT NULL DEFAULT now(),
    expires_at timestamptz NOT NULL
);
CREATE INDEX sessions_user_id ON sessions (user_id);

ALTER TABLE documents ADD COLUMN title text NOT NULL DEFAULT '' CHECK (length(title) <= 200);

CREATE TABLE doc_members (
    doc_id     text        NOT NULL REFERENCES documents (id),
    user_id    bigint      NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    role       text        NOT NULL CHECK (role IN ('owner', 'editor', 'commenter', 'viewer')),
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (doc_id, user_id)
);
CREATE INDEX doc_members_user_id ON doc_members (user_id);

-- A link gives its role to every user who opens it. As with sessions, only
-- the token's hash is stored.
CREATE TABLE share_links (
    id         bigint      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    token_hash bytea       NOT NULL UNIQUE,
    doc_id     text        NOT NULL REFERENCES documents (id),
    role       text        NOT NULL CHECK (role IN ('editor', 'commenter', 'viewer')),
    created_by bigint      NOT NULL REFERENCES users (id),
    created_at timestamptz NOT NULL DEFAULT now(),
    revoked_at timestamptz
);
CREATE INDEX share_links_doc_id ON share_links (doc_id);

-- Which user each replica number was issued to: a reconnecting client may
-- only resume its own replicas.
CREATE TABLE doc_replicas (
    doc_id     text        NOT NULL REFERENCES documents (id),
    replica    bigint      NOT NULL CHECK (replica BETWEEN 1 AND 4294967295),
    user_id    bigint      NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (doc_id, replica)
);

-- +goose Down
DROP TABLE doc_replicas;
DROP TABLE share_links;
DROP TABLE doc_members;
ALTER TABLE documents DROP COLUMN title;
DROP TABLE sessions;
DROP TABLE users;
