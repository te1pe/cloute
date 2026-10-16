package doc

import (
	"context"
	"errors"
	"time"

	"cloute/internal/crdt"
)

var (
	// ErrNoReplica is returned by Store.ReplicaOwner for a replica number
	// that was never issued.
	ErrNoReplica = errors.New("doc: no such replica")
	// ErrNoVersion is returned for a version that does not exist in the
	// document.
	ErrNoVersion = errors.New("doc: no such version")
)

// Store persists documents. It must be safe for concurrent use by
// different hubs; a single document is only ever used by one hub.
type Store interface {
	// Load returns the latest snapshot of the document and the operations
	// after it. The document must exist.
	Load(ctx context.Context, docID string) (Stored, error)
	// LoadOps returns the operations with sequence numbers above afterSeq,
	// in order.
	LoadOps(ctx context.Context, docID string, afterSeq uint64) ([]crdt.Op, error)
	// NextReplica reserves a replica number that is unique in the document
	// and records that it was issued to the user.
	NextReplica(ctx context.Context, docID string, userID int64) (uint32, error)
	// ReplicaOwner returns the user a replica number was issued to, or
	// ErrNoReplica if it was never issued.
	ReplicaOwner(ctx context.Context, docID string, replica uint32) (int64, error)
	// AppendOps stores ops with consecutive sequence numbers starting at
	// firstSeq, all or nothing.
	AppendOps(ctx context.Context, docID string, firstSeq uint64, ops []crdt.Op) error
	// SaveSnapshot stores the document state as of seq.
	SaveSnapshot(ctx context.Context, docID string, seq uint64, state []crdt.Entry) error

	// SaveVersion stores a named snapshot of the state as of seq.
	SaveVersion(ctx context.Context, docID string, v Version, state []crdt.Entry) (Version, error)
	// Versions lists the document's versions, oldest first, without state.
	Versions(ctx context.Context, docID string) ([]Version, error)
	// Version returns one version of the document with its state, or
	// ErrNoVersion.
	Version(ctx context.Context, docID string, id int64) (Version, []crdt.Entry, error)

	// SaveComment stores a new comment and returns it with ID and time.
	SaveComment(ctx context.Context, docID string, c Comment) (Comment, error)
	// Comments returns the document's unresolved comments, oldest first,
	// with author names.
	Comments(ctx context.Context, docID string) ([]Comment, error)
	// ResolveComment marks a comment resolved by the user.
	ResolveComment(ctx context.Context, docID string, id, userID int64) error
}

// Comment is a remark on the characters From..To (inclusive, document order).
type Comment struct {
	ID         int64
	From, To   crdt.ID
	Body       string
	AuthorID   int64
	AuthorName string
	CreatedAt  time.Time
	Resolved   bool
}

// Version describes a named snapshot.
type Version struct {
	ID         int64
	Name       string
	Seq        uint64 // the document as of this sequence number
	CreatedBy  int64
	AuthorName string // filled in by Versions and Version
	CreatedAt  time.Time
}

// Stored is a document as loaded from a Store.
type Stored struct {
	SnapshotSeq uint64       // 0 if there is no snapshot yet
	Snapshot    []crdt.Entry // state as of SnapshotSeq
	Ops         []crdt.Op    // operations SnapshotSeq+1, SnapshotSeq+2, ...
}
