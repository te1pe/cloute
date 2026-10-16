-- +goose Up
-- A comment covers the characters from (from_lamport, from_replica) to
-- (to_lamport, to_replica) inclusive, in document order. Anchoring to CRDT
-- IDs instead of offsets keeps the comment on its text whatever is edited
-- around it.
CREATE TABLE doc_comments (
    id           bigint      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    doc_id       text        NOT NULL REFERENCES documents (id),
    author_id    bigint      NOT NULL REFERENCES users (id),
    from_lamport bigint      NOT NULL CHECK (from_lamport > 0),
    from_replica bigint      NOT NULL CHECK (from_replica BETWEEN 0 AND 4294967295),
    to_lamport   bigint      NOT NULL CHECK (to_lamport > 0),
    to_replica   bigint      NOT NULL CHECK (to_replica BETWEEN 0 AND 4294967295),
    body         text        NOT NULL CHECK (length(body) BETWEEN 1 AND 2000),
    created_at   timestamptz NOT NULL DEFAULT now(),
    resolved_at  timestamptz,
    resolved_by  bigint      REFERENCES users (id)
);
CREATE INDEX doc_comments_active ON doc_comments (doc_id) WHERE resolved_at IS NULL;

-- +goose Down
DROP TABLE doc_comments;
