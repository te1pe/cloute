package doc_test

import (
	"errors"
	"testing"

	"cloute/internal/auth"
	"cloute/internal/crdt"
	"cloute/internal/doc"
	"cloute/internal/doc/doctest"
)

// offline types text into a replica without sending it anywhere.
func offline(t *testing.T, d *crdt.Doc, pos int, text string) []crdt.Op {
	t.Helper()
	var ops []crdt.Op
	for i, r := range []rune(text) {
		op, err := d.InsertAt(pos+i, r)
		if err != nil {
			t.Fatal(err)
		}
		ops = append(ops, op)
	}
	return ops
}

func apply(t *testing.T, d *crdt.Doc, ops []crdt.Op) {
	t.Helper()
	for _, op := range ops {
		if err := d.Apply(op); err != nil {
			t.Fatal(err)
		}
	}
}

// TestOfflineEditsMerge is the stage 4 scenario at hub level: Bob loses the
// connection, both keep editing, Bob reconnects with his unacknowledged
// operations, and nobody loses anything.
func TestOfflineEditsMerge(t *testing.T) {
	store := doctest.NewMemStore()
	r := newRegistry(t, store, testConfig)

	h, alice, wa := join(t, r, "doc")
	aliceDoc := restore(t, wa)
	typeText(t, h, alice, aliceDoc, "AB")

	_, bob, wb := join(t, r, "doc")
	bobDoc := restore(t, wb)
	bobSeq := wb.Seq
	h.Leave(bob) // connection lost

	typeText(t, h, alice, aliceDoc, "cd")                    // online, at the end
	pending := offline(t, bobDoc, 1, "xy")                   // offline, between A and B
	pending = append(pending, offline(t, bobDoc, 0, ">")...) // and at the start
	del, _ := bobDoc.DeleteAt(bobDoc.Len() - 1)              // and deletes B
	pending = append(pending, del)

	_, bob2, w := rejoin(t, r, "doc", doc.JoinRequest{Replica: bob.Replica(), LastSeq: bobSeq, Pending: pending})
	if bob2.Replica() != bob.Replica() {
		t.Fatalf("resumed replica %d, want %d", bob2.Replica(), bob.Replica())
	}
	if !w.CatchUp || len(w.Ops) != 2 || w.Seq != 4 {
		t.Fatalf("welcome = %+v, want catch-up with Alice's 2 ops up to seq 4", w)
	}
	apply(t, bobDoc, w.Ops)
	if ack := next(t, bob2).(doc.Ack); ack.Seq != 4+uint64(len(pending)) {
		t.Fatalf("ack for pending = %+v, want seq %d", ack, 4+len(pending))
	}

	upd := next(t, alice).(doc.Update)
	apply(t, aliceDoc, upd.Ops)

	// Alice: "AB" + "cd" at the end. Bob offline: "xy" after A, ">" at the
	// start, B deleted. Nothing is lost on either side.
	const want = ">Axycd"
	if aliceDoc.String() != want || bobDoc.String() != want {
		t.Fatalf("alice %q, bob %q; want both %q", aliceDoc.String(), bobDoc.String(), want)
	}
}

func TestReconnectFarBehindGetsSnapshot(t *testing.T) {
	cfg := testConfig
	cfg.MaxCatchUp = 2
	r := newRegistry(t, doctest.NewMemStore(), cfg)

	h, alice, wa := join(t, r, "doc")
	_, bob, wb := join(t, r, "doc")
	bobDoc := restore(t, wb)
	h.Leave(bob)
	typeText(t, h, alice, restore(t, wa), "abc") // 3 ops > MaxCatchUp

	pending := offline(t, bobDoc, 0, "Z")
	_, bob2, w := rejoin(t, r, "doc", doc.JoinRequest{Replica: bob.Replica(), LastSeq: wb.Seq, Pending: pending})
	if w.CatchUp {
		t.Fatal("a client 3 ops behind with MaxCatchUp 2 must get a snapshot")
	}
	// The client rebuilds from the snapshot and re-applies its own pending
	// operations, as the web client does.
	rebuilt, err := crdt.Restore(w.Replica, w.Snapshot)
	if err != nil {
		t.Fatal(err)
	}
	apply(t, rebuilt, pending)
	next(t, bob2) // ack
	if rebuilt.String() != "Zabc" {
		t.Fatalf("rebuilt text %q, want %q", rebuilt.String(), "Zabc")
	}
}

// TestResentOpsAreNotDuplicated covers a lost ack: the hub stored the batch,
// the connection broke before the ack arrived, and the client resends it.
func TestResentOpsAreNotDuplicated(t *testing.T) {
	store := doctest.NewMemStore()
	r := newRegistry(t, store, testConfig)
	h, alice, _ := join(t, r, "doc")
	_, bob, wb := join(t, r, "doc")
	bobDoc := restore(t, wb)

	ops := offline(t, bobDoc, 0, "hi")
	del, _ := bobDoc.DeleteAt(0)
	ops = append(ops, del)
	submit(t, h, bob, ops...)
	next(t, alice) // the update
	h.Leave(bob)   // the ack is lost with the connection
	if store.Seq("doc") != 3 {
		t.Fatalf("stored %d ops, want 3", store.Seq("doc"))
	}

	_, bob2, w := rejoin(t, r, "doc", doc.JoinRequest{Replica: bob.Replica(), LastSeq: wb.Seq, Pending: ops})
	apply(t, bobDoc, w.Ops) // its own ops come back in the catch-up: no-ops
	if ack := next(t, bob2).(doc.Ack); ack.Seq != 3 {
		t.Fatalf("ack = %+v, want seq 3 (nothing new stored)", ack)
	}
	if store.Seq("doc") != 3 {
		t.Fatalf("log has %d ops after resend, want 3", store.Seq("doc"))
	}
	select {
	case msg := <-alice.Messages():
		t.Fatalf("alice got %+v for a resent batch", msg)
	default:
	}
	if bobDoc.String() != "i" {
		t.Fatalf("bob has %q, want %q", bobDoc.String(), "i")
	}
}

func TestResumeUnknownReplica(t *testing.T) {
	r := newRegistry(t, doctest.NewMemStore(), testConfig)
	_, c, _ := join(t, r, "doc") // replica 1, issued to user 1

	for _, req := range []doc.JoinRequest{
		{UserID: 1, Role: auth.RoleEditor, Replica: c.Replica() + 1}, // never issued
		{UserID: 2, Role: auth.RoleEditor, Replica: c.Replica()},     // issued to someone else
	} {
		if _, _, err := r.Join(t.Context(), "doc", req); !errors.Is(err, doc.ErrUnknownReplica) {
			t.Fatalf("Join(%+v): %v, want ErrUnknownReplica", req, err)
		}
	}
}

func TestResumeTakesOverStaleConnection(t *testing.T) {
	r := newRegistry(t, doctest.NewMemStore(), testConfig)
	_, old, w := join(t, r, "doc") // the server has not noticed it is dead
	_, fresh, _ := rejoin(t, r, "doc", doc.JoinRequest{Replica: old.Replica(), LastSeq: w.Seq})

	if err := waitClosed(t, old); !errors.Is(err, doc.ErrReplaced) {
		t.Fatalf("old connection dropped with %v, want ErrReplaced", err)
	}
	if fresh.Replica() != old.Replica() {
		t.Fatalf("resumed replica %d, want %d", fresh.Replica(), old.Replica())
	}
}

func TestResumeAfterServerRestart(t *testing.T) {
	store := doctest.NewMemStore()
	r1 := newRegistry(t, store, testConfig)
	h, bob, w := join(t, r1, "doc")
	bobDoc := restore(t, w)
	typeText(t, h, bob, bobDoc, "ok")
	r1.Close()

	pending := offline(t, bobDoc, 2, "!")
	r2 := newRegistry(t, store, testConfig)
	_, bob2, w2 := rejoin(t, r2, "doc", doc.JoinRequest{Replica: bob.Replica(), LastSeq: 2, Pending: pending})
	if !w2.CatchUp || len(w2.Ops) != 0 {
		t.Fatalf("welcome = %+v, want an empty catch-up", w2)
	}
	if ack := next(t, bob2).(doc.Ack); ack.Seq != 3 {
		t.Fatalf("ack = %+v, want seq 3", ack)
	}
}
