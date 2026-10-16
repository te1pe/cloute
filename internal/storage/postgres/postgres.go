// Package postgres stores documents in PostgreSQL: a log of operations
// (doc_ops) plus periodic snapshots of the CRDT state (doc_snapshots).
package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"github.com/pressly/goose/v3/lock"

	"cloute/internal/crdt"
	"cloute/internal/crdt/wire"
	"cloute/internal/doc"
	"cloute/migrations"
)

// Connect opens a connection pool and checks that the database answers.
func Connect(ctx context.Context, url string) (*pgxpool.Pool, error) {
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		return nil, fmt.Errorf("postgres: parse config: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("postgres: connect: %w", err)
	}
	return pool, nil
}

// Migrate applies pending migrations from the migrations directory. An
// advisory lock makes concurrent callers (several server instances or test
// processes) apply them one at a time.
func Migrate(ctx context.Context, pool *pgxpool.Pool) ([]*goose.MigrationResult, error) {
	locker, err := lock.NewPostgresSessionLocker()
	if err != nil {
		return nil, fmt.Errorf("postgres: migration lock: %w", err)
	}
	db := stdlib.OpenDBFromPool(pool)
	defer func() { _ = db.Close() }()

	provider, err := goose.NewProvider(goose.DialectPostgres, db, migrations.FS, goose.WithSessionLocker(locker))
	if err != nil {
		return nil, fmt.Errorf("postgres: migrations: %w", err)
	}
	results, err := provider.Up(ctx)
	if err != nil {
		return nil, fmt.Errorf("postgres: migrate: %w", err)
	}
	return results, nil
}

// Store implements doc.Store.
type Store struct {
	pool *pgxpool.Pool
}

var _ doc.Store = (*Store)(nil)

func NewStore(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

func (s *Store) Load(ctx context.Context, docID string) (doc.Stored, error) {
	// A repeatable-read transaction sees the snapshot and the log at the
	// same point in time.
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead})
	if err != nil {
		return doc.Stored{}, fmt.Errorf("postgres: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var exists bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM documents WHERE id = $1)`, docID).Scan(&exists); err != nil {
		return doc.Stored{}, fmt.Errorf("postgres: load document: %w", err)
	}
	if !exists {
		return doc.Stored{}, fmt.Errorf("postgres: no document %q", docID)
	}

	var stored doc.Stored

	var snapSeq int64
	var state []byte
	err = tx.QueryRow(ctx,
		`SELECT seq, state FROM doc_snapshots WHERE doc_id = $1 ORDER BY seq DESC LIMIT 1`,
		docID).Scan(&snapSeq, &state)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
	case err != nil:
		return doc.Stored{}, fmt.Errorf("postgres: load snapshot: %w", err)
	default:
		var ws wire.Snapshot
		if err := json.Unmarshal(state, &ws); err != nil {
			return doc.Stored{}, fmt.Errorf("postgres: snapshot %d: %w", snapSeq, err)
		}
		if stored.Snapshot, err = wire.DecodeSnapshot(ws); err != nil {
			return doc.Stored{}, fmt.Errorf("postgres: snapshot %d: %w", snapSeq, err)
		}
		stored.SnapshotSeq = uint64(snapSeq)
	}

	if stored.Ops, err = loadOps(ctx, tx, docID, stored.SnapshotSeq); err != nil {
		return doc.Stored{}, err
	}
	return stored, tx.Commit(ctx)
}

func (s *Store) LoadOps(ctx context.Context, docID string, afterSeq uint64) ([]crdt.Op, error) {
	return loadOps(ctx, s.pool, docID, afterSeq)
}

// loadOps reads the log after afterSeq and checks that it has no gaps.
func loadOps(ctx context.Context, q interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
}, docID string, afterSeq uint64) ([]crdt.Op, error) {
	rows, err := q.Query(ctx,
		`SELECT seq, op FROM doc_ops WHERE doc_id = $1 AND seq > $2 ORDER BY seq`,
		docID, int64(afterSeq))
	if err != nil {
		return nil, fmt.Errorf("postgres: load ops: %w", err)
	}
	var ops []crdt.Op
	want := int64(afterSeq) + 1
	var seq int64
	var raw []byte
	_, err = pgx.ForEachRow(rows, []any{&seq, &raw}, func() error {
		if seq != want {
			return fmt.Errorf("log has a gap: seq %d where %d expected", seq, want)
		}
		want++
		var w wire.Op
		if err := json.Unmarshal(raw, &w); err != nil {
			return fmt.Errorf("op %d: %w", seq, err)
		}
		op, err := wire.DecodeOp(w)
		if err != nil {
			return fmt.Errorf("op %d: %w", seq, err)
		}
		ops = append(ops, op)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("postgres: load ops: %w", err)
	}
	return ops, nil
}

func (s *Store) NextReplica(ctx context.Context, docID string, userID int64) (uint32, error) {
	var replica int64
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		// The row lock taken by UPDATE serializes concurrent reservations.
		err := tx.QueryRow(ctx,
			`UPDATE documents SET last_replica = last_replica + 1 WHERE id = $1 RETURNING last_replica`,
			docID).Scan(&replica)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx,
			`INSERT INTO doc_replicas (doc_id, replica, user_id) VALUES ($1, $2, $3)`,
			docID, replica, userID)
		return err
	})
	if err != nil {
		return 0, fmt.Errorf("postgres: next replica: %w", err)
	}
	return uint32(replica), nil // the column's CHECK keeps it within uint32
}

func (s *Store) ReplicaOwner(ctx context.Context, docID string, replica uint32) (int64, error) {
	var userID int64
	err := s.pool.QueryRow(ctx,
		`SELECT user_id FROM doc_replicas WHERE doc_id = $1 AND replica = $2`,
		docID, int64(replica)).Scan(&userID)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, doc.ErrNoReplica
	}
	if err != nil {
		return 0, fmt.Errorf("postgres: replica owner: %w", err)
	}
	return userID, nil
}

func (s *Store) AppendOps(ctx context.Context, docID string, firstSeq uint64, ops []crdt.Op) error {
	seqs := make([]int64, len(ops))
	payloads := make([]string, len(ops))
	for i, op := range ops {
		data, err := json.Marshal(wire.EncodeOp(op))
		if err != nil {
			return fmt.Errorf("postgres: encode op: %w", err)
		}
		seqs[i] = int64(firstSeq) + int64(i)
		payloads[i] = string(data)
	}
	// One statement, so the batch is stored all or nothing. The primary key
	// (doc_id, seq) rejects a sequence number that is already taken.
	_, err := s.pool.Exec(ctx,
		`INSERT INTO doc_ops (doc_id, seq, op)
		 SELECT $1, t.seq, t.op::jsonb FROM unnest($2::bigint[], $3::text[]) AS t(seq, op)`,
		docID, seqs, payloads)
	if err != nil {
		return fmt.Errorf("postgres: append ops: %w", err)
	}
	return nil
}

func (s *Store) SaveSnapshot(ctx context.Context, docID string, seq uint64, state []crdt.Entry) error {
	data, err := json.Marshal(wire.EncodeSnapshot(state))
	if err != nil {
		return fmt.Errorf("postgres: encode snapshot: %w", err)
	}
	_, err = s.pool.Exec(ctx,
		`INSERT INTO doc_snapshots (doc_id, seq, state) VALUES ($1, $2, $3::text::jsonb)
		 ON CONFLICT (doc_id, seq) DO NOTHING`,
		docID, int64(seq), string(data))
	if err != nil {
		return fmt.Errorf("postgres: save snapshot: %w", err)
	}
	return nil
}

func (s *Store) SaveVersion(ctx context.Context, docID string, v doc.Version, state []crdt.Entry) (doc.Version, error) {
	data, err := json.Marshal(wire.EncodeSnapshot(state))
	if err != nil {
		return doc.Version{}, fmt.Errorf("postgres: encode version: %w", err)
	}
	err = s.pool.QueryRow(ctx,
		`INSERT INTO doc_versions (doc_id, name, seq, state, created_by) VALUES ($1, $2, $3, $4::text::jsonb, $5)
		 RETURNING id, created_at`,
		docID, v.Name, int64(v.Seq), string(data), v.CreatedBy).Scan(&v.ID, &v.CreatedAt)
	if err != nil {
		return doc.Version{}, fmt.Errorf("postgres: save version: %w", err)
	}
	return v, nil
}

const versionColumns = `v.id, v.name, v.seq, v.created_by, u.name, v.created_at`

func scanVersion(row pgx.Row, extra ...any) (doc.Version, error) {
	var v doc.Version
	var seq int64
	err := row.Scan(append([]any{&v.ID, &v.Name, &seq, &v.CreatedBy, &v.AuthorName, &v.CreatedAt}, extra...)...)
	v.Seq = uint64(seq)
	return v, err
}

func (s *Store) Versions(ctx context.Context, docID string) ([]doc.Version, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT `+versionColumns+` FROM doc_versions v JOIN users u ON u.id = v.created_by
		 WHERE v.doc_id = $1 ORDER BY v.created_at, v.id`,
		docID)
	if err != nil {
		return nil, fmt.Errorf("postgres: versions: %w", err)
	}
	versions, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (doc.Version, error) {
		return scanVersion(row)
	})
	if err != nil {
		return nil, fmt.Errorf("postgres: versions: %w", err)
	}
	return versions, nil
}

func (s *Store) Version(ctx context.Context, docID string, id int64) (doc.Version, []crdt.Entry, error) {
	var state []byte
	v, err := scanVersion(s.pool.QueryRow(ctx,
		`SELECT `+versionColumns+`, v.state FROM doc_versions v JOIN users u ON u.id = v.created_by
		 WHERE v.doc_id = $1 AND v.id = $2`,
		docID, id), &state)
	if errors.Is(err, pgx.ErrNoRows) {
		return doc.Version{}, nil, doc.ErrNoVersion
	}
	if err != nil {
		return doc.Version{}, nil, fmt.Errorf("postgres: version: %w", err)
	}
	var ws wire.Snapshot
	if err := json.Unmarshal(state, &ws); err != nil {
		return doc.Version{}, nil, fmt.Errorf("postgres: version %d: %w", id, err)
	}
	entries, err := wire.DecodeSnapshot(ws)
	if err != nil {
		return doc.Version{}, nil, fmt.Errorf("postgres: version %d: %w", id, err)
	}
	return v, entries, nil
}

func (s *Store) SaveComment(ctx context.Context, docID string, c doc.Comment) (doc.Comment, error) {
	err := s.pool.QueryRow(ctx,
		`INSERT INTO doc_comments (doc_id, author_id, from_lamport, from_replica, to_lamport, to_replica, body)
		 VALUES ($1, $2, $3, $4, $5, $6, $7) RETURNING id, created_at`,
		docID, c.AuthorID, int64(c.From.Lamport), int64(c.From.Replica), int64(c.To.Lamport), int64(c.To.Replica), c.Body,
	).Scan(&c.ID, &c.CreatedAt)
	if err != nil {
		return doc.Comment{}, fmt.Errorf("postgres: save comment: %w", err)
	}
	return c, nil
}

func (s *Store) Comments(ctx context.Context, docID string) ([]doc.Comment, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT c.id, c.from_lamport, c.from_replica, c.to_lamport, c.to_replica, c.body, c.author_id, u.name, c.created_at
		 FROM doc_comments c JOIN users u ON u.id = c.author_id
		 WHERE c.doc_id = $1 AND c.resolved_at IS NULL ORDER BY c.id`,
		docID)
	if err != nil {
		return nil, fmt.Errorf("postgres: comments: %w", err)
	}
	comments, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (doc.Comment, error) {
		var c doc.Comment
		var fl, fr, tl, tr int64
		err := row.Scan(&c.ID, &fl, &fr, &tl, &tr, &c.Body, &c.AuthorID, &c.AuthorName, &c.CreatedAt)
		c.From = crdt.ID{Lamport: uint64(fl), Replica: uint32(fr)}
		c.To = crdt.ID{Lamport: uint64(tl), Replica: uint32(tr)}
		return c, err
	})
	if err != nil {
		return nil, fmt.Errorf("postgres: comments: %w", err)
	}
	return comments, nil
}

func (s *Store) ResolveComment(ctx context.Context, docID string, id, userID int64) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE doc_comments SET resolved_at = now(), resolved_by = $3
		 WHERE doc_id = $1 AND id = $2 AND resolved_at IS NULL`,
		docID, id, userID)
	if err != nil {
		return fmt.Errorf("postgres: resolve comment: %w", err)
	}
	return nil
}
