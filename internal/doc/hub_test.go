package doc_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"cloute/internal/auth"
	"cloute/internal/crdt"
	"cloute/internal/doc"
	"cloute/internal/doc/doctest"
)

var testConfig = doc.Config{
	QueueSize:     16,
	IdleTimeout:   time.Minute,
	SnapshotEvery: 1000,
	MaxCatchUp:    1000,
	StoreTimeout:  5 * time.Second,
}

func newRegistry(t *testing.T, store doc.Store, cfg doc.Config) *doc.Registry {
	t.Helper()
	r := doc.NewRegistry(store, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	t.Cleanup(r.Close)
	return r
}

func join(t *testing.T, r *doc.Registry, docID string) (*doc.Hub, *doc.Client, doc.Welcome) {
	t.Helper()
	return rejoin(t, r, docID, doc.JoinRequest{})
}

// rejoin joins with req; a zero UserID or Role means user 1 as an editor.
func rejoin(t *testing.T, r *doc.Registry, docID string, req doc.JoinRequest) (*doc.Hub, *doc.Client, doc.Welcome) {
	t.Helper()
	if req.UserID == 0 {
		req.UserID = 1
	}
	if req.Role == "" {
		req.Role = auth.RoleEditor
	}
	h, c, err := r.Join(t.Context(), docID, req)
	if err != nil {
		t.Fatalf("Join: %v", err)
	}
	return h, c, next(t, c).(doc.Welcome)
}

// next returns the next message from the client's queue.
func next(t *testing.T, c *doc.Client) any {
	t.Helper()
	select {
	case msg, ok := <-c.Messages():
		if !ok {
			t.Fatalf("queue closed unexpectedly: %v", c.Err())
		}
		return msg
	case <-time.After(5 * time.Second):
		t.Fatal("no message within 5s")
		return nil
	}
}

// waitClosed drains the client's queue until the hub closes it.
func waitClosed(t *testing.T, c *doc.Client) error {
	t.Helper()
	timeout := time.After(5 * time.Second)
	for {
		select {
		case _, ok := <-c.Messages():
			if !ok {
				return c.Err()
			}
		case <-timeout:
			t.Fatal("queue not closed within 5s")
		}
	}
}

func submit(t *testing.T, h *doc.Hub, c *doc.Client, ops ...crdt.Op) {
	t.Helper()
	if err := h.Submit(t.Context(), c, ops); err != nil {
		t.Fatalf("Submit: %v", err)
	}
}

// typeText makes the client type text at the end of its replica and waits
// for the ack of every character.
func typeText(t *testing.T, h *doc.Hub, c *doc.Client, local *crdt.Doc, text string) {
	t.Helper()
	for _, r := range text {
		op, err := local.InsertAt(local.Len(), r)
		if err != nil {
			t.Fatal(err)
		}
		submit(t, h, c, op)
		if _, ok := next(t, c).(doc.Ack); !ok {
			t.Fatal("want ack")
		}
	}
}

func restore(t *testing.T, w doc.Welcome) *crdt.Doc {
	t.Helper()
	if w.CatchUp {
		t.Fatal("want a snapshot welcome, got catch-up")
	}
	d, err := crdt.Restore(w.Replica, w.Snapshot)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func TestHubWelcomeAndBroadcast(t *testing.T) {
	r := newRegistry(t, doctest.NewMemStore(), testConfig)

	h, a, wa := join(t, r, "doc")
	_, b, _ := join(t, r, "doc")
	if a.Replica() == b.Replica() || a.Replica() == 0 || b.Replica() == 0 {
		t.Fatalf("replicas must be unique and non-zero: %d, %d", a.Replica(), b.Replica())
	}
	if wa.Seq != 0 || len(wa.Snapshot) != 0 || wa.Replica != a.Replica() {
		t.Fatalf("welcome for empty doc = %+v", wa)
	}

	local := crdt.New(a.Replica())
	op1, _ := local.InsertAt(0, 'h')
	op2, _ := local.InsertAt(1, 'i')
	submit(t, h, a, op1, op2)

	if ack := next(t, a).(doc.Ack); ack.Seq != 2 {
		t.Fatalf("ack = %+v, want seq 2", ack)
	}
	if upd := next(t, b).(doc.Update); upd.Seq != 2 || len(upd.Ops) != 2 {
		t.Fatalf("update = %+v, want seq 2 with 2 ops", upd)
	}

	// A late joiner receives the document as a snapshot.
	_, c, wc := join(t, r, "doc")
	if wc.Seq != 2 || restore(t, wc).String() != "hi" {
		t.Fatalf("late welcome: seq %d, text %q", wc.Seq, restore(t, wc).String())
	}
	if c.Replica() <= b.Replica() {
		t.Fatalf("replica %d reused", c.Replica())
	}
}

func TestDocumentSurvivesReload(t *testing.T) {
	store := doctest.NewMemStore()
	cfg := testConfig
	cfg.SnapshotEvery = 3

	r1 := doc.NewRegistry(store, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	h, a, w := join(t, r1, "doc")
	typeText(t, h, a, restore(t, w), "hello") // snapshot after op 3, ops 4-5 in the log
	if got := store.SnapshotSeq("doc"); got != 3 {
		t.Fatalf("periodic snapshot at seq %d, want 3", got)
	}

	// Load in a second registry while the first is still running: this is
	// what a crash looks like (no final snapshot), the tail is replayed.
	r2 := newRegistry(t, store, cfg)
	_, b, wb := join(t, r2, "doc")
	if wb.Seq != 5 || restore(t, wb).String() != "hello" {
		t.Fatalf("after crash: seq %d, text %q; want 5, %q", wb.Seq, restore(t, wb).String(), "hello")
	}
	if b.Replica() <= a.Replica() {
		t.Fatalf("replica %d reused after reload (old %d)", b.Replica(), a.Replica())
	}

	// A clean shutdown saves a final snapshot.
	r1.Close()
	if got := store.SnapshotSeq("doc"); got != 5 {
		t.Fatalf("final snapshot at seq %d, want 5", got)
	}
}

func TestHubUnloadsWhenIdle(t *testing.T) {
	store := doctest.NewMemStore()
	cfg := testConfig
	cfg.IdleTimeout = 50 * time.Millisecond
	r := newRegistry(t, store, cfg)

	h, a, w := join(t, r, "doc")
	typeText(t, h, a, restore(t, w), "abc")
	h.Leave(a)

	deadline := time.Now().Add(5 * time.Second)
	for store.SnapshotSeq("doc") != 3 {
		if time.Now().After(deadline) {
			t.Fatal("idle hub did not unload and save a snapshot")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := h.Join(t.Context(), doc.JoinRequest{}); !errors.Is(err, doc.ErrHubClosed) {
		t.Fatalf("Join on unloaded hub: %v, want ErrHubClosed", err)
	}

	// The registry starts a fresh hub that loads the document.
	h2, _, w2 := join(t, r, "doc")
	if h2 == h || restore(t, w2).String() != "abc" {
		t.Fatalf("reloaded hub: same=%v, text %q", h2 == h, restore(t, w2).String())
	}
}

func TestHubStopsOnStorageFailure(t *testing.T) {
	store := doctest.NewMemStore()
	r := newRegistry(t, store, testConfig)
	h, a, w := join(t, r, "doc")
	_, b, _ := join(t, r, "doc")
	local := restore(t, w)
	typeText(t, h, a, local, "ok")
	next(t, b)
	next(t, b)

	store.SetFailure(errors.New("disk on fire"))
	op, _ := local.InsertAt(2, '!')
	submit(t, h, a, op)

	// Nobody gets an ack or an update for the lost op: everyone is
	// disconnected and reconnects to a hub loaded from the store.
	for _, c := range []*doc.Client{a, b} {
		if err := waitClosed(t, c); !errors.Is(err, doc.ErrStorage) {
			t.Fatalf("client dropped with %v, want ErrStorage", err)
		}
	}
	if store.SnapshotSeq("doc") != 0 {
		t.Fatal("a failed hub must not save a snapshot")
	}
	if _, _, err := r.Join(t.Context(), "doc", doc.JoinRequest{}); !errors.Is(err, doc.ErrStorage) {
		t.Fatalf("Join while the store is down: %v, want ErrStorage", err)
	}

	store.SetFailure(nil)
	_, _, w2 := join(t, r, "doc")
	if w2.Seq != 2 || restore(t, w2).String() != "ok" {
		t.Fatalf("after recovery: seq %d, text %q; want 2, %q", w2.Seq, restore(t, w2).String(), "ok")
	}
}

func TestHubRejectsInvalidOps(t *testing.T) {
	tests := []struct {
		name string
		op   func(own uint32) crdt.Op
	}{
		{"foreign replica", func(own uint32) crdt.Op {
			return crdt.Insert{ID: crdt.ID{Lamport: 2, Replica: own + 1}, After: crdt.ID{}, Char: 'x'}
		}},
		{"unknown after", func(own uint32) crdt.Op {
			return crdt.Insert{ID: crdt.ID{Lamport: 9, Replica: own}, After: crdt.ID{Lamport: 8, Replica: own}, Char: 'x'}
		}},
		{"delete of unknown", func(own uint32) crdt.Op {
			return crdt.Delete{ID: crdt.ID{Lamport: 7, Replica: own}}
		}},
		{"conflicting duplicate (rejected by the CRDT)", func(own uint32) crdt.Op {
			return crdt.Insert{ID: crdt.ID{Lamport: 1, Replica: own}, After: crdt.ID{}, Char: 'z'}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := doctest.NewMemStore()
			r := newRegistry(t, store, testConfig)
			h, a, _ := join(t, r, "doc")
			_, b, _ := join(t, r, "doc")

			// The valid prefix of the batch is stored and broadcast,
			// the invalid op is refused and the sender is dropped.
			valid := crdt.Insert{ID: crdt.ID{Lamport: 1, Replica: a.Replica()}, After: crdt.ID{}, Char: 'a'}
			submit(t, h, a, valid, tt.op(a.Replica()))

			if err := waitClosed(t, a); !errors.Is(err, doc.ErrInvalidOp) {
				t.Fatalf("sender dropped with %v, want ErrInvalidOp", err)
			}
			if upd := next(t, b).(doc.Update); upd.Seq != 1 || len(upd.Ops) != 1 || upd.Ops[0] != valid {
				t.Fatalf("update = %+v, want only the valid op with seq 1", upd)
			}
			if store.Seq("doc") != 1 {
				t.Fatalf("stored %d ops, want 1", store.Seq("doc"))
			}
		})
	}
}

func TestHubDropsSlowClient(t *testing.T) {
	cfg := testConfig
	cfg.QueueSize = 2
	r := newRegistry(t, doctest.NewMemStore(), cfg)
	h, writer, w := join(t, r, "doc")
	_, slow, err := r.Join(t.Context(), "doc", doc.JoinRequest{UserID: 1, Role: auth.RoleViewer}) // never reads; its queue holds the welcome
	if err != nil {
		t.Fatal(err)
	}

	// Queue of 2: welcome + 1 update fit, the second update overflows.
	typeText(t, h, writer, restore(t, w), "xyz")
	if err := waitClosed(t, slow); !errors.Is(err, doc.ErrSlowClient) {
		t.Fatalf("slow client dropped with %v, want ErrSlowClient", err)
	}
}

func TestHubLeave(t *testing.T) {
	r := newRegistry(t, doctest.NewMemStore(), testConfig)
	h, a, _ := join(t, r, "doc")
	h.Leave(a)
	if err := waitClosed(t, a); err != nil {
		t.Fatalf("Err after Leave = %v, want nil", err)
	}
	h.Leave(a) // second Leave is a no-op
}

func TestRegistryClose(t *testing.T) {
	r := doc.NewRegistry(doctest.NewMemStore(), testConfig, slog.New(slog.NewTextHandler(io.Discard, nil)))
	h, a, _ := join(t, r, "doc")

	r.Close()
	if err := waitClosed(t, a); !errors.Is(err, doc.ErrHubClosed) {
		t.Fatalf("client dropped with %v, want ErrHubClosed", err)
	}
	if _, err := h.Join(context.Background(), doc.JoinRequest{}); !errors.Is(err, doc.ErrHubClosed) {
		t.Fatalf("Join after Close: %v, want ErrHubClosed", err)
	}
	if _, _, err := r.Join(context.Background(), "other", doc.JoinRequest{}); !errors.Is(err, doc.ErrHubClosed) {
		t.Fatalf("Join on a new doc after Close: %v, want ErrHubClosed", err)
	}
	if err := h.Submit(context.Background(), a, nil); !errors.Is(err, doc.ErrHubClosed) {
		t.Fatalf("Submit after Close: %v, want ErrHubClosed", err)
	}
	h.Leave(a) // must not block
	r.Close()  // idempotent
}
