package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"cloute/internal/auth"
)

// AuthStore implements auth.Store.
type AuthStore struct {
	pool *pgxpool.Pool
}

var _ auth.Store = (*AuthStore)(nil)

func NewAuthStore(pool *pgxpool.Pool) *AuthStore {
	return &AuthStore{pool: pool}
}

// roleRank orders roles in SQL the same way auth.Role.AtLeast does.
const roleRank = `array_position(ARRAY['viewer', 'commenter', 'editor', 'owner'], %s)`

func (s *AuthStore) CreateUser(ctx context.Context, email, name string, passwordHash []byte) (auth.User, error) {
	u := auth.User{Email: email, Name: name}
	err := s.pool.QueryRow(ctx,
		`INSERT INTO users (email, name, password_hash) VALUES ($1, $2, $3) RETURNING id`,
		email, name, passwordHash).Scan(&u.ID)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" { // unique_violation on email
		return auth.User{}, auth.ErrEmailTaken
	}
	if err != nil {
		return auth.User{}, fmt.Errorf("postgres: create user: %w", err)
	}
	return u, nil
}

func (s *AuthStore) UserByEmail(ctx context.Context, email string) (auth.User, []byte, error) {
	var u auth.User
	var hash []byte
	err := s.pool.QueryRow(ctx,
		`SELECT id, email, name, password_hash FROM users WHERE email = $1`,
		email).Scan(&u.ID, &u.Email, &u.Name, &hash)
	if errors.Is(err, pgx.ErrNoRows) {
		return auth.User{}, nil, auth.ErrNotFound
	}
	if err != nil {
		return auth.User{}, nil, fmt.Errorf("postgres: user by email: %w", err)
	}
	return u, hash, nil
}

func (s *AuthStore) CreateSession(ctx context.Context, tokenHash []byte, userID int64, expires time.Time) error {
	_, err := s.pool.Exec(ctx,
		`INSERT INTO sessions (token_hash, user_id, expires_at) VALUES ($1, $2, $3)`,
		tokenHash, userID, expires)
	if err != nil {
		return fmt.Errorf("postgres: create session: %w", err)
	}
	return nil
}

func (s *AuthStore) SessionUser(ctx context.Context, tokenHash []byte, now time.Time) (auth.User, error) {
	var u auth.User
	err := s.pool.QueryRow(ctx,
		`SELECT u.id, u.email, u.name FROM sessions s JOIN users u ON u.id = s.user_id
		 WHERE s.token_hash = $1 AND s.expires_at > $2`,
		tokenHash, now).Scan(&u.ID, &u.Email, &u.Name)
	if errors.Is(err, pgx.ErrNoRows) {
		return auth.User{}, auth.ErrNotFound
	}
	if err != nil {
		return auth.User{}, fmt.Errorf("postgres: session: %w", err)
	}
	return u, nil
}

func (s *AuthStore) DeleteSession(ctx context.Context, tokenHash []byte) error {
	if _, err := s.pool.Exec(ctx, `DELETE FROM sessions WHERE token_hash = $1`, tokenHash); err != nil {
		return fmt.Errorf("postgres: delete session: %w", err)
	}
	return nil
}

func (s *AuthStore) CreateDocument(ctx context.Context, docID, title string, owner int64) error {
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO documents (id, title) VALUES ($1, $2)`, docID, title); err != nil {
			return err
		}
		_, err := tx.Exec(ctx,
			`INSERT INTO doc_members (doc_id, user_id, role) VALUES ($1, $2, 'owner')`, docID, owner)
		return err
	})
	if err != nil {
		return fmt.Errorf("postgres: create document: %w", err)
	}
	return nil
}

func (s *AuthStore) Document(ctx context.Context, docID string, userID int64) (auth.Document, error) {
	d, err := document(ctx, s.pool, docID, userID)
	if errors.Is(err, pgx.ErrNoRows) {
		return auth.Document{}, auth.ErrNotFound
	}
	if err != nil {
		return auth.Document{}, fmt.Errorf("postgres: document: %w", err)
	}
	return d, nil
}

// userRole is the role of user $2 in document d, NULL if none: owner if
// they own it, otherwise the highest role among the unrevoked links of the
// document they have opened. Access through a link ends with the link.
var userRole = `CASE
	WHEN EXISTS (SELECT 1 FROM doc_members o WHERE o.doc_id = d.id AND o.user_id = $2 AND o.role = 'owner')
	THEN 'owner'
	ELSE (SELECT l.role FROM link_joins j JOIN share_links l ON l.id = j.link_id
	      WHERE l.doc_id = d.id AND j.user_id = $2 AND l.revoked_at IS NULL
	      ORDER BY ` + fmt.Sprintf(roleRank, "l.role") + ` DESC LIMIT 1)
	END`

func document(ctx context.Context, q interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, docID string, userID int64) (auth.Document, error) {
	var d auth.Document
	var role *string
	err := q.QueryRow(ctx,
		`SELECT d.id, d.title, `+userRole+` FROM documents d WHERE d.id = $1`,
		docID, userID).Scan(&d.ID, &d.Title, &role)
	if err == nil && role == nil {
		return auth.Document{}, pgx.ErrNoRows // exists, but not for this user
	}
	if role != nil {
		d.Role = auth.Role(*role)
	}
	return d, err
}

func (s *AuthStore) Documents(ctx context.Context, userID int64) ([]auth.Document, error) {
	// Candidates are the documents the user owns or has opened a link of;
	// userRole then drops those whose links are all revoked.
	rows, err := s.pool.Query(ctx,
		`SELECT id, title, role FROM (
		   SELECT d.id, d.title, d.created_at, `+userRole+` AS role FROM documents d
		   WHERE d.id IN (SELECT doc_id FROM doc_members WHERE user_id = $1 AND role = 'owner'
		                  UNION
		                  SELECT l.doc_id FROM link_joins j JOIN share_links l ON l.id = j.link_id WHERE j.user_id = $1)
		 ) x WHERE role IS NOT NULL ORDER BY created_at DESC, id`,
		userID, userID)
	if err != nil {
		return nil, fmt.Errorf("postgres: documents: %w", err)
	}
	docs, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (auth.Document, error) {
		var d auth.Document
		err := row.Scan(&d.ID, &d.Title, &d.Role)
		return d, err
	})
	if err != nil {
		return nil, fmt.Errorf("postgres: documents: %w", err)
	}
	return docs, nil
}

func (s *AuthStore) CreateLink(ctx context.Context, tokenHash []byte, docID string, role auth.Role, createdBy int64) (auth.Link, error) {
	l := auth.Link{Role: role}
	err := s.pool.QueryRow(ctx,
		`INSERT INTO share_links (token_hash, doc_id, role, created_by) VALUES ($1, $2, $3, $4)
		 RETURNING id, created_at`,
		tokenHash, docID, string(role), createdBy).Scan(&l.ID, &l.CreatedAt)
	if err != nil {
		return auth.Link{}, fmt.Errorf("postgres: create link: %w", err)
	}
	return l, nil
}

func (s *AuthStore) Links(ctx context.Context, docID string) ([]auth.Link, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT id, role, created_at FROM share_links
		 WHERE doc_id = $1 AND revoked_at IS NULL ORDER BY created_at, id`,
		docID)
	if err != nil {
		return nil, fmt.Errorf("postgres: links: %w", err)
	}
	links, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (auth.Link, error) {
		var l auth.Link
		err := row.Scan(&l.ID, &l.Role, &l.CreatedAt)
		return l, err
	})
	if err != nil {
		return nil, fmt.Errorf("postgres: links: %w", err)
	}
	return links, nil
}

func (s *AuthStore) RevokeLink(ctx context.Context, docID string, linkID int64) ([]int64, error) {
	var users []int64
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx,
			`UPDATE share_links SET revoked_at = now() WHERE doc_id = $1 AND id = $2 AND revoked_at IS NULL`,
			docID, linkID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return auth.ErrNotFound
		}
		rows, err := tx.Query(ctx, `SELECT user_id FROM link_joins WHERE link_id = $1`, linkID)
		if err != nil {
			return err
		}
		users, err = pgx.CollectRows(rows, pgx.RowTo[int64])
		return err
	})
	if errors.Is(err, auth.ErrNotFound) {
		return nil, err
	}
	if err != nil {
		return nil, fmt.Errorf("postgres: revoke link: %w", err)
	}
	return users, nil
}

func (s *AuthStore) JoinByLink(ctx context.Context, tokenHash []byte, userID int64) (auth.Document, error) {
	var d auth.Document
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var linkID int64
		var docID string
		err := tx.QueryRow(ctx,
			`SELECT id, doc_id FROM share_links WHERE token_hash = $1 AND revoked_at IS NULL`,
			tokenHash).Scan(&linkID, &docID)
		if err != nil {
			return err
		}
		// Only the fact is stored; the role follows from the links. Opening
		// a weaker link never lowers a role: the highest one counts.
		_, err = tx.Exec(ctx,
			`INSERT INTO link_joins (link_id, user_id) VALUES ($1, $2) ON CONFLICT DO NOTHING`,
			linkID, userID)
		if err != nil {
			return err
		}
		d, err = document(ctx, tx, docID, userID)
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return auth.Document{}, auth.ErrNotFound
	}
	if err != nil {
		return auth.Document{}, fmt.Errorf("postgres: join by link: %w", err)
	}
	return d, nil
}
