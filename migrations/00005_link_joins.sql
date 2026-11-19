-- +goose Up
CREATE TABLE link_joins (
    link_id   bigint      NOT NULL REFERENCES share_links (id),
    user_id   bigint      NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    joined_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (link_id, user_id)
);
CREATE INDEX link_joins_user_id ON link_joins (user_id);

INSERT INTO link_joins (link_id, user_id)
SELECT l.id, m.user_id
FROM doc_members m
JOIN share_links l ON l.doc_id = m.doc_id AND l.role = m.role
WHERE m.role <> 'owner'
ON CONFLICT DO NOTHING;

-- +goose Down
DROP TABLE link_joins;
