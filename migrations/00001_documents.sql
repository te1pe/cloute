-- +goose Up
CREATE TABLE documents (
    id           text        PRIMARY KEY CHECK (id ~ '^[A-Za-z0-9_-]{1,64}$'),
    -- Last replica number given to a client; replicas are unique per document.
    last_replica bigint      NOT NULL DEFAULT 0 CHECK (last_replica BETWEEN 0 AND 4294967295),
    created_at   timestamptz NOT NULL DEFAULT now()
);

-- Every accepted operation, in the order the hub accepted it.
CREATE TABLE doc_ops (
    doc_id text   NOT NULL REFERENCES documents (id),
    seq    bigint NOT NULL CHECK (seq > 0),
    op     jsonb  NOT NULL,
    PRIMARY KEY (doc_id, seq)
);

-- Full CRDT state as of seq; loading = latest snapshot + ops after it.
CREATE TABLE doc_snapshots (
    doc_id     text        NOT NULL REFERENCES documents (id),
    seq        bigint      NOT NULL CHECK (seq > 0),
    state      jsonb       NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (doc_id, seq)
);

-- +goose Down
DROP TABLE doc_snapshots;
DROP TABLE doc_ops;
DROP TABLE documents;
