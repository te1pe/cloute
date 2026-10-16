package postgres_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"

	"cloute/internal/crdt"
	"cloute/internal/doc"
	"cloute/internal/storage/postgres"
	"cloute/internal/storage/postgres/pgtest"
)

func typed(t *testing.T, d *crdt.Doc, text string) []crdt.Op {
	t.Helper()
	var ops []crdt.Op
	for _, r := range text {
		op, err := d.InsertAt(d.Len(), r)
		if err != nil {
			t.Fatal(err)
		}
		ops = append(ops, op)
	}
	return ops
}

func TestLoadNewDocument(t *testing.T) {
	pool := pgtest.Pool(t)
	store := postgres.NewStore(pool)
	id, _ := pgtest.Document(t, pool)
	stored, err := store.Load(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	if stored.SnapshotSeq != 0 || len(stored.Snapshot) != 0 || len(stored.Ops) != 0 {
		t.Fatalf("new document is not empty: %+v", stored)
	}
}

func TestLoadUnknownDocument(t *testing.T) {
	store := postgres.NewStore(pgtest.Pool(t))
	if _, err := store.Load(t.Context(), pgtest.DocID()); err == nil {
		t.Fatal("Load of a document that was never created succeeded")
	}
}

func TestSnapshotPlusTail(t *testing.T) {
	ctx := t.Context()
	pool := pgtest.Pool(t)
	store := postgres.NewStore(pool)
	id, _ := pgtest.Document(t, pool)

	d := crdt.New(1)
	head := typed(t, d, "Привет")
	if err := store.AppendOps(ctx, id, 1, head); err != nil {
		t.Fatal(err)
	}
	del, _ := d.DeleteAt(0)
	if err := store.AppendOps(ctx, id, 7, []crdt.Op{del}); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveSnapshot(ctx, id, 7, d.Snapshot()); err != nil {
		t.Fatal(err)
	}
	tail := typed(t, d, ", мир 👋")
	if err := store.AppendOps(ctx, id, 8, tail); err != nil {
		t.Fatal(err)
	}

	stored, err := store.Load(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if stored.SnapshotSeq != 7 || len(stored.Ops) != len(tail) {
		t.Fatalf("loaded snapshot at %d with %d ops, want 7 and %d", stored.SnapshotSeq, len(stored.Ops), len(tail))
	}
	if !slices.Equal(stored.Ops, tail) {
		t.Fatalf("tail ops differ:\n got %+v\nwant %+v", stored.Ops, tail)
	}

	replay, err := crdt.Restore(0, stored.Snapshot)
	if err != nil {
		t.Fatal(err)
	}
	for _, op := range stored.Ops {
		if err := replay.Apply(op); err != nil {
			t.Fatal(err)
		}
	}
	if replay.String() != "ривет, мир 👋" || !slices.Equal(replay.Snapshot(), d.Snapshot()) {
		t.Fatalf("replayed %q, want %q with identical state", replay.String(), d.String())
	}
}

func TestAppendOpsRejectsTakenSeq(t *testing.T) {
	ctx := t.Context()
	pool := pgtest.Pool(t)
	store := postgres.NewStore(pool)
	id, _ := pgtest.Document(t, pool)
	ops := typed(t, crdt.New(1), "abc")
	if err := store.AppendOps(ctx, id, 1, ops); err != nil {
		t.Fatal(err)
	}

	// Overlapping batch: the primary key rejects it as a whole.
	more := typed(t, crdt.New(2), "xy")
	if err := store.AppendOps(ctx, id, 3, more); err == nil {
		t.Fatal("append at a taken seq succeeded")
	}
	stored, err := store.Load(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if len(stored.Ops) != 3 {
		t.Fatalf("log has %d ops after a rejected batch, want 3", len(stored.Ops))
	}
}

func TestNextReplicaIsUniqueUnderConcurrency(t *testing.T) {
	ctx := t.Context()
	pool := pgtest.Pool(t)
	store := postgres.NewStore(pool)
	id, user := pgtest.Document(t, pool)

	const n = 20
	var mu sync.Mutex
	seen := make(map[uint32]bool)
	var wg sync.WaitGroup
	for range n {
		wg.Go(func() {
			r, err := store.NextReplica(ctx, id, user)
			if err != nil {
				t.Error(err)
				return
			}
			mu.Lock()
			seen[r] = true
			mu.Unlock()
		})
	}
	wg.Wait()
	if len(seen) != n || seen[0] {
		t.Fatalf("got %d distinct replicas %v, want %d, none of them 0", len(seen), seen, n)
	}
}

func TestNextReplicaOfUnknownDocument(t *testing.T) {
	store := postgres.NewStore(pgtest.Pool(t))
	if _, err := store.NextReplica(t.Context(), pgtest.DocID(), 1); err == nil {
		t.Fatal("NextReplica of a document that was never created succeeded")
	}
}

func TestReplicaOwner(t *testing.T) {
	ctx := t.Context()
	pool := pgtest.Pool(t)
	store := postgres.NewStore(pool)
	id, user := pgtest.Document(t, pool)

	r, err := store.NextReplica(ctx, id, user)
	if err != nil {
		t.Fatal(err)
	}
	if owner, err := store.ReplicaOwner(ctx, id, r); err != nil || owner != user {
		t.Fatalf("ReplicaOwner(%d) = %d, %v; want %d", r, owner, err, user)
	}
	if _, err := store.ReplicaOwner(ctx, id, r+1); !errors.Is(err, doc.ErrNoReplica) {
		t.Fatalf("ReplicaOwner of an unissued replica: %v, want ErrNoReplica", err)
	}
}

func TestInvalidDocumentIDIsRejected(t *testing.T) {
	pool := pgtest.Pool(t)
	_, owner := pgtest.Document(t, pool)
	err := postgres.NewAuthStore(pool).CreateDocument(t.Context(), "../"+strings.Repeat("x", 10), "t", owner)
	if err == nil || !strings.Contains(err.Error(), "documents_id_check") {
		t.Fatalf("Load with an invalid id: %v, want a CHECK violation", err)
	}
}

func TestMigrateIsIdempotent(t *testing.T) {
	pool := pgtest.Pool(t) // already migrated by pgtest
	results, err := postgres.Migrate(context.Background(), pool)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 0 {
		t.Fatalf("second migration applied %d migrations, want 0", len(results))
	}
}
