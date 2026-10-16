// Package doctest provides an in-memory doc.Store for tests.
package doctest

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"time"

	"cloute/internal/crdt"
	"cloute/internal/doc"
)

// MemStore is a doc.Store kept in memory. Like the database, it rejects
// operations whose sequence numbers do not continue the log.
type MemStore struct {
	mu         sync.Mutex
	docs       map[string]*memDoc
	fail       error
	failAppend error
}

type memVersion struct {
	v     doc.Version
	state []crdt.Entry
}

type memDoc struct {
	comments    []doc.Comment
	versions    []memVersion
	replicas    map[uint32]int64 // replica -> user
	lastReplica uint32
	ops         []crdt.Op // ops[i] has seq i+1
	snapSeq     uint64
	snapshot    []crdt.Entry
}

var _ doc.Store = (*MemStore)(nil)

func NewMemStore() *MemStore {
	return &MemStore{docs: make(map[string]*memDoc)}
}

// SetFailure makes every following call fail with err; nil restores normal
// operation.
func (s *MemStore) SetFailure(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fail = err
}

// SetAppendFailure makes only AppendOps fail with err; nil restores it.
func (s *MemStore) SetAppendFailure(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failAppend = err
}

// Seq returns the sequence number of the last stored operation.
func (s *MemStore) Seq(docID string) uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	if d, ok := s.docs[docID]; ok {
		return uint64(len(d.ops))
	}
	return 0
}

// SnapshotSeq returns the sequence number of the latest snapshot.
func (s *MemStore) SnapshotSeq(docID string) uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	if d, ok := s.docs[docID]; ok {
		return d.snapSeq
	}
	return 0
}

func (s *MemStore) Load(_ context.Context, docID string) (doc.Stored, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fail != nil {
		return doc.Stored{}, s.fail
	}
	// Unlike the database, documents spring into existence on first load:
	// hub tests do not deal with document creation.
	d, ok := s.docs[docID]
	if !ok {
		d = &memDoc{replicas: make(map[uint32]int64)}
		s.docs[docID] = d
	}
	return doc.Stored{
		SnapshotSeq: d.snapSeq,
		Snapshot:    slices.Clone(d.snapshot),
		Ops:         slices.Clone(d.ops[d.snapSeq:]),
	}, nil
}

func (s *MemStore) LoadOps(_ context.Context, docID string, afterSeq uint64) ([]crdt.Op, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fail != nil {
		return nil, s.fail
	}
	d, ok := s.docs[docID]
	if !ok || afterSeq > uint64(len(d.ops)) {
		return nil, fmt.Errorf("doctest: no ops after %d in %q", afterSeq, docID)
	}
	return slices.Clone(d.ops[afterSeq:]), nil
}

func (s *MemStore) NextReplica(_ context.Context, docID string, userID int64) (uint32, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fail != nil {
		return 0, s.fail
	}
	d, ok := s.docs[docID]
	if !ok {
		return 0, fmt.Errorf("doctest: no document %q", docID)
	}
	d.lastReplica++
	d.replicas[d.lastReplica] = userID
	return d.lastReplica, nil
}

func (s *MemStore) ReplicaOwner(_ context.Context, docID string, replica uint32) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fail != nil {
		return 0, s.fail
	}
	d, ok := s.docs[docID]
	if !ok {
		return 0, fmt.Errorf("doctest: no document %q", docID)
	}
	user, ok := d.replicas[replica]
	if !ok {
		return 0, doc.ErrNoReplica
	}
	return user, nil
}

func (s *MemStore) AppendOps(_ context.Context, docID string, firstSeq uint64, ops []crdt.Op) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fail != nil {
		return s.fail
	}
	if s.failAppend != nil {
		return s.failAppend
	}
	d, ok := s.docs[docID]
	if !ok {
		return fmt.Errorf("doctest: no document %q", docID)
	}
	if want := uint64(len(d.ops)) + 1; firstSeq != want {
		return fmt.Errorf("doctest: append at seq %d, log continues at %d", firstSeq, want)
	}
	d.ops = append(d.ops, ops...)
	return nil
}

func (s *MemStore) SaveSnapshot(_ context.Context, docID string, seq uint64, state []crdt.Entry) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fail != nil {
		return s.fail
	}
	d, ok := s.docs[docID]
	if !ok {
		return fmt.Errorf("doctest: no document %q", docID)
	}
	if seq > uint64(len(d.ops)) {
		return fmt.Errorf("doctest: snapshot at seq %d beyond the log (%d)", seq, len(d.ops))
	}
	if seq > d.snapSeq {
		d.snapSeq = seq
		d.snapshot = slices.Clone(state)
	}
	return nil
}

func (s *MemStore) SaveVersion(_ context.Context, docID string, v doc.Version, state []crdt.Entry) (doc.Version, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fail != nil {
		return doc.Version{}, s.fail
	}
	d, ok := s.docs[docID]
	if !ok {
		return doc.Version{}, fmt.Errorf("doctest: no document %q", docID)
	}
	v.ID = int64(len(d.versions) + 1)
	v.CreatedAt = time.Now()
	d.versions = append(d.versions, memVersion{v: v, state: slices.Clone(state)})
	return v, nil
}

func (s *MemStore) Versions(_ context.Context, docID string) ([]doc.Version, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fail != nil {
		return nil, s.fail
	}
	var out []doc.Version
	if d, ok := s.docs[docID]; ok {
		for _, mv := range d.versions {
			out = append(out, mv.v)
		}
	}
	return out, nil
}

func (s *MemStore) Version(_ context.Context, docID string, id int64) (doc.Version, []crdt.Entry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fail != nil {
		return doc.Version{}, nil, s.fail
	}
	d, ok := s.docs[docID]
	if !ok || id < 1 || id > int64(len(d.versions)) {
		return doc.Version{}, nil, doc.ErrNoVersion
	}
	mv := d.versions[id-1]
	return mv.v, slices.Clone(mv.state), nil
}

func (s *MemStore) SaveComment(_ context.Context, docID string, c doc.Comment) (doc.Comment, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fail != nil {
		return doc.Comment{}, s.fail
	}
	d, ok := s.docs[docID]
	if !ok {
		return doc.Comment{}, fmt.Errorf("doctest: no document %q", docID)
	}
	c.ID = int64(len(d.comments) + 1)
	c.CreatedAt = time.Now()
	d.comments = append(d.comments, c)
	return c, nil
}

func (s *MemStore) Comments(_ context.Context, docID string) ([]doc.Comment, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fail != nil {
		return nil, s.fail
	}
	var out []doc.Comment
	if d, ok := s.docs[docID]; ok {
		for _, c := range d.comments {
			if !c.Resolved {
				out = append(out, c)
			}
		}
	}
	return out, nil
}

func (s *MemStore) ResolveComment(_ context.Context, docID string, id, _ int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fail != nil {
		return s.fail
	}
	d, ok := s.docs[docID]
	if !ok || id < 1 || id > int64(len(d.comments)) {
		return fmt.Errorf("doctest: no comment %d", id)
	}
	d.comments[id-1].Resolved = true
	return nil
}
