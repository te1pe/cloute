-- +goose Up
-- Access through links is derived, not stored: a user's role in a document
-- is "owner" if they own it, otherwise the highest role among the links of
-- the document they have opened and that are not revoked. Revoking a link
-- therefore takes back what it gave, and nothing has to be deleted.
--
-- From now on doc_members is read for owners only; its other rows stay as
-- history of the old scheme.
CREATE TABLE link_joins (
    link_id   bigint      NOT NULL REFERENCES share_links (id),
    user_id   bigint      NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    joined_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (link_id, user_id)
);
CREATE INDEX link_joins_user_id ON link_joins (user_id);

-- Members who joined before: the scheme did not record which link they
-- used, but it was one of the document's links with their role. Tie them to
-- all of those; if all of those are revoked, the access ends now, as it
-- should have when they were revoked.
INSERT INTO link_joins (link_id, user_id)
SELECT l.id, m.user_id
FROM doc_members m
JOIN share_links l ON l.doc_id = m.doc_id AND l.role = m.role
WHERE m.role <> 'owner'
ON CONFLICT DO NOTHING;

-- +goose Down
DROP TABLE link_joins;
