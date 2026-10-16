-- +goose Up
-- A version is a named snapshot of the CRDT state, kept forever. Restoring
-- it does not touch this table or the log: it appends new operations.
CREATE TABLE doc_versions (
    id         bigint      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    doc_id     text        NOT NULL REFERENCES documents (id),
    name       text        NOT NULL CHECK (length(name) BETWEEN 1 AND 100),
    seq        bigint      NOT NULL CHECK (seq >= 0),
    state      jsonb       NOT NULL,
    created_by bigint      NOT NULL REFERENCES users (id),
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX doc_versions_doc_id ON doc_versions (doc_id, created_at);

-- +goose Down
DROP TABLE doc_versions;
