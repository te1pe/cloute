package doc_test

import (
	"errors"
	"testing"

	"cloute/internal/auth"
	"cloute/internal/doc"
	"cloute/internal/doc/doctest"
)

// TestRestoreKeepsConcurrentEdit is the stage 6 acceptance test at hub
// level: Alice restores a version while Bob, who has not seen the restore
// yet, types. Bob's edit survives and both end up with the same text.
func TestRestoreKeepsConcurrentEdit(t *testing.T) {
	store := doctest.NewMemStore()
	r := newRegistry(t, store, testConfig)

	h, alice, wa := join(t, r, "doc")
	aliceDoc := restore(t, wa)
	typeText(t, h, alice, aliceDoc, "Версия 1")
	v, err := r.SaveVersion(t.Context(), "doc", 1, "  первая  ")
	if err != nil {
		t.Fatal(err)
	}
	if v.Name != "первая" || v.Seq != 8 {
		t.Fatalf("version = %+v, want name %q at seq 8", v, "первая")
	}
	typeText(t, h, alice, aliceDoc, ", черновик")

	_, bob, wb := rejoin(t, r, "doc", doc.JoinRequest{UserID: 2, Role: auth.RoleEditor})
	bobDoc := restore(t, wb)
	excl, _ := bobDoc.InsertAt(bobDoc.Len(), '!') // typed before the restore reaches Bob

	seq, err := r.RestoreVersion(t.Context(), "doc", 1, v.ID)
	if err != nil {
		t.Fatal(err)
	}
	submit(t, h, bob, excl) // arrives after the restore

	// Alice gets the restore, then Bob's edit; Bob gets the restore, then
	// the ack of his edit.
	rev := next(t, alice).(doc.Update)
	if rev.Seq != seq {
		t.Fatalf("restore update at seq %d, Restore returned %d", rev.Seq, seq)
	}
	apply(t, aliceDoc, rev.Ops)
	if aliceDoc.String() != "Версия 1" {
		t.Fatalf("after restore: %q", aliceDoc.String())
	}
	apply(t, aliceDoc, next(t, alice).(doc.Update).Ops)

	apply(t, bobDoc, next(t, bob).(doc.Update).Ops)
	if ack := next(t, bob).(doc.Ack); ack.Seq != seq+1 {
		t.Fatalf("bob's ack = %+v, want seq %d", ack, seq+1)
	}

	const want = "Версия 1!"
	if aliceDoc.String() != want || bobDoc.String() != want {
		t.Fatalf("alice %q, bob %q; want both %q", aliceDoc.String(), bobDoc.String(), want)
	}

	// The restore is ordinary history: the log grew, nothing was rewritten,
	// and a newcomer sees the merged text.
	if store.Seq("doc") != seq+1 || seq <= 18 {
		t.Fatalf("log has %d ops, restore ended at %d", store.Seq("doc"), seq)
	}
	_, _, w := join(t, r, "doc")
	if got := restore(t, w).String(); got != want {
		t.Fatalf("newcomer sees %q", got)
	}
}

func TestRestoreWithoutClients(t *testing.T) {
	store := doctest.NewMemStore()
	r := newRegistry(t, store, testConfig)
	h, c, w := join(t, r, "doc")
	local := restore(t, w)
	typeText(t, h, c, local, "old")
	v, err := r.SaveVersion(t.Context(), "doc", 1, "v")
	if err != nil {
		t.Fatal(err)
	}
	typeText(t, h, c, local, "new")
	h.Leave(c)

	// A restore loads the hub if needed and works with nobody connected.
	if _, err := r.RestoreVersion(t.Context(), "doc", 1, v.ID); err != nil {
		t.Fatal(err)
	}
	_, _, w2 := join(t, r, "doc")
	if got := restore(t, w2).String(); got != "old" {
		t.Fatalf("text %q, want %q", got, "old")
	}

	_, text, err := r.VersionText(t.Context(), "doc", v.ID)
	if err != nil || text != "old" {
		t.Fatalf("VersionText = %q, %v", text, err)
	}
}

func TestVersionErrors(t *testing.T) {
	r := newRegistry(t, doctest.NewMemStore(), testConfig)
	join(t, r, "doc")

	for _, name := range []string{"", "   ", string(make([]rune, 101))} {
		if _, err := r.SaveVersion(t.Context(), "doc", 1, name); !errors.Is(err, doc.ErrInvalidName) {
			t.Errorf("SaveVersion(%q): %v, want ErrInvalidName", name, err)
		}
	}
	if _, err := r.RestoreVersion(t.Context(), "doc", 1, 42); !errors.Is(err, doc.ErrNoVersion) {
		t.Fatalf("restore of a missing version: %v, want ErrNoVersion", err)
	}
	if _, _, err := r.VersionText(t.Context(), "doc", 42); !errors.Is(err, doc.ErrNoVersion) {
		t.Fatalf("text of a missing version: %v, want ErrNoVersion", err)
	}
}

// TestRestoreStorageFailure: if the restore's operations cannot be stored,
// the hub stops instead of serving state the store does not have, and the
// document comes back as the store has it.
func TestRestoreStorageFailure(t *testing.T) {
	store := doctest.NewMemStore()
	r := newRegistry(t, store, testConfig)
	h, c, w := join(t, r, "doc")
	local := restore(t, w)
	typeText(t, h, c, local, "ab")
	v, err := r.SaveVersion(t.Context(), "doc", 1, "v")
	if err != nil {
		t.Fatal(err)
	}
	typeText(t, h, c, local, "c")

	store.SetAppendFailure(errors.New("disk on fire"))
	if _, err := r.RestoreVersion(t.Context(), "doc", 1, v.ID); !errors.Is(err, doc.ErrStorage) {
		t.Fatalf("restore with a broken store: %v, want ErrStorage", err)
	}
	if err := waitClosed(t, c); !errors.Is(err, doc.ErrStorage) {
		t.Fatalf("client dropped with %v, want ErrStorage", err)
	}

	store.SetAppendFailure(nil)
	_, _, w2 := join(t, r, "doc")
	if got := restore(t, w2).String(); got != "abc" {
		t.Fatalf("after the failed restore: %q, want the stored %q", got, "abc")
	}
}
