package crdt

import (
	"errors"
	"testing"
	"unicode/utf8"
)

// checkInvariants verifies internal bookkeeping that the public API relies on.
func checkInvariants(t *testing.T, d *Doc) {
	t.Helper()
	if got := utf8.RuneCountInString(d.String()); got != d.Len() {
		t.Fatalf("Len() = %d, but text %q has %d runes", d.Len(), d.String(), got)
	}
	inList := 0
	for n := d.head.next; n != nil; n = n.next {
		inList++
		if d.nodes[n.id] != n {
			t.Fatalf("node %v is in the list but not in the index", n.id)
		}
	}
	if inList != len(d.nodes)-1 { // the index also holds head
		t.Fatalf("list has %d nodes, index has %d", inList, len(d.nodes)-1)
	}
	buffered := 0
	for _, ops := range d.pending {
		buffered += len(ops)
	}
	if buffered != d.Pending() {
		t.Fatalf("Pending() = %d, buffer holds %d", d.Pending(), buffered)
	}
}

func mustApply(t *testing.T, d *Doc, ops ...Op) {
	t.Helper()
	for _, op := range ops {
		if err := d.Apply(op); err != nil {
			t.Fatalf("Apply(%+v): %v", op, err)
		}
	}
}

func ins(lamport uint64, replica uint32, after ID, char rune) Insert {
	return Insert{ID: ID{lamport, replica}, After: after, Char: char}
}

func TestIDLess(t *testing.T) {
	tests := []struct {
		a, b ID
		want bool
	}{
		{ID{1, 1}, ID{2, 1}, true},
		{ID{2, 1}, ID{1, 1}, false},
		{ID{1, 1}, ID{1, 2}, true},
		{ID{1, 2}, ID{1, 1}, false},
		{ID{1, 9}, ID{2, 1}, true}, // Lamport dominates Replica
		{ID{1, 1}, ID{1, 1}, false},
	}
	for _, tt := range tests {
		if got := tt.a.Less(tt.b); got != tt.want {
			t.Errorf("%v.Less(%v) = %v, want %v", tt.a, tt.b, got, tt.want)
		}
	}
}

func TestLocalEdits(t *testing.T) {
	d := New(1)
	for i, r := range "hlo" {
		if _, err := d.InsertAt(i, r); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := d.InsertAt(1, 'e'); err != nil {
		t.Fatal(err)
	}
	if _, err := d.InsertAt(3, 'l'); err != nil {
		t.Fatal(err)
	}
	if got := d.String(); got != "hello" {
		t.Fatalf("text = %q, want %q", got, "hello")
	}

	del, err := d.DeleteAt(0)
	if err != nil {
		t.Fatal(err)
	}
	if del.ID != (ID{1, 1}) {
		t.Errorf("DeleteAt(0) deleted %v, want the first inserted character {1 1}", del.ID)
	}
	if _, err := d.DeleteAt(3); err != nil {
		t.Fatal(err)
	}
	if got := d.String(); got != "ell" {
		t.Fatalf("text = %q, want %q", got, "ell")
	}
	checkInvariants(t, d)
}

func TestLocalInsertIDs(t *testing.T) {
	d := New(7)
	a, _ := d.InsertAt(0, 'a')
	b, _ := d.InsertAt(1, 'b')
	if a != ins(1, 7, ID{}, 'a') {
		t.Errorf("first insert = %+v", a)
	}
	if b != ins(2, 7, a.ID, 'b') {
		t.Errorf("second insert = %+v", b)
	}

	// A remote op with a higher timestamp advances the local clock.
	mustApply(t, d, ins(10, 3, b.ID, 'x'))
	c, _ := d.InsertAt(0, 'c')
	if c.ID != (ID{11, 7}) {
		t.Errorf("insert after observing Lamport 10 got ID %v, want {11 7}", c.ID)
	}
}

func TestContains(t *testing.T) {
	a := ins(1, 1, ID{}, 'a')
	b := ins(2, 1, a.ID, 'b')
	d := New(2)
	mustApply(t, d, b) // buffered: not applied yet

	if !d.Contains(ID{}) {
		t.Error("head must always be present")
	}
	if d.Contains(b.ID) {
		t.Error("buffered insert must not be reported as present")
	}
	mustApply(t, d, a, Delete{ID: a.ID})
	if !d.Contains(a.ID) || !d.Contains(b.ID) {
		t.Error("applied characters, including tombstones, must be present")
	}
}

func TestApplied(t *testing.T) {
	a := ins(1, 1, ID{}, 'a')
	del := Delete{ID: a.ID}
	d := New(2)
	if d.Applied(a) || d.Applied(del) {
		t.Fatal("nothing is applied in an empty doc")
	}
	mustApply(t, d, del) // buffered: the target is unknown
	if d.Applied(del) {
		t.Fatal("a buffered delete is not applied")
	}
	mustApply(t, d, a) // releases the delete
	if !d.Applied(a) || !d.Applied(del) {
		t.Fatal("insert and delete must be reported as applied")
	}
}

func TestInOrder(t *testing.T) {
	d := New(1)
	var ids []ID
	for i, r := range "abc" {
		op, _ := d.InsertAt(i, r)
		ids = append(ids, op.ID)
	}
	if _, err := d.DeleteAt(1); err != nil { // b becomes a tombstone
		t.Fatal(err)
	}
	a, b, c := ids[0], ids[1], ids[2]
	for _, tt := range []struct {
		x, y ID
		want bool
	}{
		{a, c, true}, {c, a, false}, {a, a, true}, {b, c, true}, {c, b, false},
		{a, ID{9, 9}, false}, {ID{9, 9}, a, false},
	} {
		if got := d.InOrder(tt.x, tt.y); got != tt.want {
			t.Errorf("InOrder(%v, %v) = %v, want %v", tt.x, tt.y, got, tt.want)
		}
	}
}

func TestLocalEditsOutOfRange(t *testing.T) {
	d := New(1)
	if _, err := d.InsertAt(1, 'a'); !errors.Is(err, ErrOutOfRange) {
		t.Errorf("InsertAt(1) on empty doc: err = %v, want ErrOutOfRange", err)
	}
	if _, err := d.InsertAt(-1, 'a'); !errors.Is(err, ErrOutOfRange) {
		t.Errorf("InsertAt(-1): err = %v, want ErrOutOfRange", err)
	}
	if _, err := d.DeleteAt(0); !errors.Is(err, ErrOutOfRange) {
		t.Errorf("DeleteAt(0) on empty doc: err = %v, want ErrOutOfRange", err)
	}
	if _, err := d.InsertAt(0, 0xD800); !errors.Is(err, ErrInvalidOp) {
		t.Errorf("InsertAt with a surrogate: err = %v, want ErrInvalidOp", err)
	}
}

func TestTombstones(t *testing.T) {
	d := New(1)
	a := ins(1, 1, ID{}, 'a')
	b := ins(2, 1, a.ID, 'b')
	mustApply(t, d, a, b, Delete{ID: a.ID})

	if got := d.String(); got != "b" {
		t.Fatalf("text = %q, want %q", got, "b")
	}
	n, ok := d.nodes[a.ID]
	if !ok || !n.deleted {
		t.Fatalf("deleted character must stay in the sequence as a tombstone")
	}

	// A concurrent insert that refers to the deleted character still has a
	// place to go and is applied immediately, not buffered.
	mustApply(t, d, ins(3, 2, a.ID, 'x'))
	if d.Pending() != 0 {
		t.Fatalf("insert after a tombstone was buffered")
	}
	if got := d.String(); got != "xb" {
		t.Fatalf("text = %q, want %q", got, "xb")
	}

	// Positions in local edits count visible characters only.
	if _, err := d.InsertAt(0, '>'); err != nil {
		t.Fatal(err)
	}
	if got := d.String(); got != ">xb" {
		t.Fatalf("text = %q, want %q", got, ">xb")
	}
	checkInvariants(t, d)
}

func TestIdempotence(t *testing.T) {
	a := ins(1, 1, ID{}, 'a')
	b := ins(2, 1, a.ID, 'b')
	ops := []Op{a, b, Delete{ID: a.ID}}

	d := New(2)
	for range 3 {
		mustApply(t, d, ops...)
		if got := d.String(); got != "b" {
			t.Fatalf("text = %q, want %q", got, "b")
		}
		if d.Len() != 1 {
			t.Fatalf("Len() = %d, want 1", d.Len())
		}
	}
	checkInvariants(t, d)
}

func TestBuffering(t *testing.T) {
	t.Run("insert waits for its After", func(t *testing.T) {
		a := ins(1, 1, ID{}, 'a')
		b := ins(2, 1, a.ID, 'b')
		c := ins(3, 1, b.ID, 'c')

		d := New(2)
		mustApply(t, d, c, b)
		if d.String() != "" || d.Pending() != 2 {
			t.Fatalf("text = %q, pending = %d; want empty text and 2 pending", d.String(), d.Pending())
		}
		checkInvariants(t, d)

		mustApply(t, d, a)
		if d.String() != "abc" || d.Pending() != 0 {
			t.Fatalf("text = %q, pending = %d; want %q and 0 pending", d.String(), d.Pending(), "abc")
		}
		checkInvariants(t, d)
	})

	t.Run("delete waits for its target", func(t *testing.T) {
		a := ins(1, 1, ID{}, 'a')
		d := New(2)
		mustApply(t, d, Delete{ID: a.ID})
		if d.Pending() != 1 {
			t.Fatalf("pending = %d, want 1", d.Pending())
		}
		mustApply(t, d, a)
		if d.String() != "" || d.Pending() != 0 || d.Len() != 0 {
			t.Fatalf("text = %q, pending = %d; want empty and 0 pending", d.String(), d.Pending())
		}
		checkInvariants(t, d)
	})

	t.Run("duplicate buffered op is stored once", func(t *testing.T) {
		a := ins(1, 1, ID{}, 'a')
		b := ins(2, 1, a.ID, 'b')
		d := New(2)
		mustApply(t, d, b, b, Delete{ID: b.ID}, Delete{ID: b.ID})
		if d.Pending() != 2 {
			t.Fatalf("pending = %d, want 2", d.Pending())
		}
		mustApply(t, d, a)
		if d.String() != "a" || d.Pending() != 0 {
			t.Fatalf("text = %q, pending = %d; want %q and 0 pending", d.String(), d.Pending(), "a")
		}
		checkInvariants(t, d)
	})
}

func TestApplyRejectsInvalidOps(t *testing.T) {
	a := ins(2, 1, ID{}, 'a')
	tests := []struct {
		name string
		op   Op
	}{
		{"zero insert ID", ins(0, 1, ID{}, 'x')},
		{"Lamport not greater than After", ins(2, 2, a.ID, 'x')},
		{"Lamport less than After", ins(1, 2, a.ID, 'x')},
		{"invalid rune", ins(3, 2, a.ID, 0xD800)},
		{"conflicting duplicate", ins(2, 1, ID{}, 'z')},
		{"delete of head", Delete{ID: ID{}}},
		{"nil op", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := New(3)
			mustApply(t, d, a)
			if err := d.Apply(tt.op); !errors.Is(err, ErrInvalidOp) {
				t.Fatalf("Apply(%+v): err = %v, want ErrInvalidOp", tt.op, err)
			}
			if d.String() != "a" || d.Pending() != 0 {
				t.Fatalf("rejected op changed the doc: text = %q, pending = %d", d.String(), d.Pending())
			}
		})
	}
}
