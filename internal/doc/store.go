package doc

import (
	"context"
	"errors"
	"time"

	"cloute/internal/crdt"
)

var (
	ErrNoReplica = errors.New("doc: no such replica")
	ErrNoVersion = errors.New("doc: no such version")
)

type Store interface {
	Load(ctx context.Context, docID string) (Stored, error)
	LoadOps(ctx context.Context, docID string, afterSeq uint64) ([]crdt.Op, error)
	NextReplica(ctx context.Context, docID string, userID int64) (uint32, error)
	ReplicaOwner(ctx context.Context, docID string, replica uint32) (int64, error)
	AppendOps(ctx context.Context, docID string, firstSeq uint64, ops []crdt.Op) error
	SaveSnapshot(ctx context.Context, docID string, seq uint64, state []crdt.Entry) error

	SaveVersion(ctx context.Context, docID string, v Version, state []crdt.Entry) (Version, error)
	Versions(ctx context.Context, docID string) ([]Version, error)
	Version(ctx context.Context, docID string, id int64) (Version, []crdt.Entry, error)

	SaveComment(ctx context.Context, docID string, c Comment) (Comment, error)
	Comments(ctx context.Context, docID string) ([]Comment, error)
	ResolveComment(ctx context.Context, docID string, id, userID int64) error
}

type Comment struct {
	ID         int64
	From, To   crdt.ID
	Body       string
	AuthorID   int64
	AuthorName string
	CreatedAt  time.Time
	Resolved   bool
}

type Version struct {
	ID         int64
	Name       string
	Seq        uint64
	CreatedBy  int64
	AuthorName string
	CreatedAt  time.Time
}

type Stored struct {
	SnapshotSeq uint64
	Snapshot    []crdt.Entry
	Ops         []crdt.Op
}
