package crdt

import (
	"errors"
	"fmt"
	"math/rand/v2"
	"strings"
	"testing"
)

func revert(t *testing.T, d *Doc, version []Entry, replica uint32) []Op {
	t.Helper()
	ops, err := d.RevertTo(version, replica)
	if err != nil {
		t.Fatal(err)
	}
	return ops
}

func TestRevertExamples(t *testing.T) {
	d := New(1)
	for i, r := range "abc" {
		if _, err := d.InsertAt(i, r); err != nil {
			t.Fatal(err)
		}
	}
	version := d.Snapshot() // "abc"

	// Delete b, insert X after a, append Y: "aXcY".
	if _, err := d.DeleteAt(1); err != nil {
		t.Fatal(err)
	}
	if _, err := d.InsertAt(1, 'X'); err != nil {
		t.Fatal(err)
	}
	if _, err := d.InsertAt(3, 'Y'); err != nil {
		t.Fatal(err)
	}
	if d.String() != "aXcY" {
		t.Fatalf("setup: %q", d.String())
	}

	ops := revert(t, d, version, 9)
	// X and Y are deleted, b comes back as one copy; a and c are untouched.
	var deletes, inserts int
	for _, op := range ops {
		switch op := op.(type) {
		case Delete:
			deletes++
		case Insert:
			inserts++
			if op.ID.Replica != 9 || op.Char != 'b' {
				t.Errorf("unexpected insert %+v", op)
			}
		}
	}
	if deletes != 2 || inserts != 1 {
		t.Fatalf("ops = %+v, want 2 deletes and 1 insert", ops)
	}
	mustApply(t, d, ops...)
	if d.String() != "abc" {
		t.Fatalf("after revert: %q, want %q", d.String(), "abc")
	}
	checkInvariants(t, d)

	// Reverting to the state we are in changes nothing.
	if ops := revert(t, d, d.Snapshot(), 9); len(ops) != 0 {
		t.Fatalf("revert to the current state: %+v", ops)
	}
}

// TestRevertKeepsAnchors shows why the revert changes as little as
// possible: a concurrent insert between two characters that the version
// shares with the current text stays exactly there. Deleting everything and
// inserting a copy of the version would move it after the copy: "abcX".
func TestRevertKeepsAnchors(t *testing.T) {
	a := New(1)
	var history []Op
	for i, r := range "abc" {
		op, err := a.InsertAt(i, r)
		if err != nil {
			t.Fatal(err)
		}
		history = append(history, op)
	}
	version := a.Snapshot()
	d, _ := a.InsertAt(3, 'd')
	history = append(history, d)

	b := New(2)
	mustApply(t, b, history...)
	x, _ := b.InsertAt(2, 'X') // between b and c, not seeing the revert

	rev := revert(t, a, version, 3)
	mustApply(t, a, rev...)
	mustApply(t, a, x)
	mustApply(t, b, rev...)
	if a.String() != "abXc" || b.String() != "abXc" {
		t.Fatalf("a %q, b %q; want both %q", a.String(), b.String(), "abXc")
	}
}

func TestRevertToEmptyAndFromEmpty(t *testing.T) {
	d := New(1)
	empty := d.Snapshot()
	for i, r := range "hi" {
		if _, err := d.InsertAt(i, r); err != nil {
			t.Fatal(err)
		}
	}
	full := d.Snapshot()

	mustApply(t, d, revert(t, d, empty, 2)...)
	if d.String() != "" {
		t.Fatalf("revert to empty: %q", d.String())
	}
	mustApply(t, d, revert(t, d, full, 2)...)
	if d.String() != "hi" {
		t.Fatalf("revert to full: %q", d.String())
	}
}

func TestRevertRejectsForeignVersion(t *testing.T) {
	other := New(1)
	if _, err := other.InsertAt(0, 'z'); err != nil {
		t.Fatal(err)
	}
	if _, err := New(2).RevertTo(other.Snapshot(), 3); !errors.Is(err, ErrInvalidOp) {
		t.Fatalf("revert to another document's snapshot: %v, want ErrInvalidOp", err)
	}
}

// TestRevertKeepsConcurrentEdits is the stage 6 property at CRDT level: the
// revert runs on one replica while another replica, which has not seen it,
// keeps editing. After exchanging everything, both replicas converge, the
// revert took effect, and every character the other replica inserted
// concurrently is still visible.
func TestRevertKeepsConcurrentEdits(t *testing.T) {
	for seed := range uint64(200) {
		t.Run(fmt.Sprintf("seed=%d", seed), func(t *testing.T) {
			rng := rand.New(rand.NewPCG(seed, 6))
			a, b := New(1), New(2)

			editRandomly := func(d *Doc, n int, alphabet []rune) []Op {
				var ops []Op
				for range n {
					if d.Len() > 0 && rng.IntN(3) == 0 {
						op, _ := d.DeleteAt(rng.IntN(d.Len()))
						ops = append(ops, op)
					} else {
						op, _ := d.InsertAt(rng.IntN(d.Len()+1), alphabet[rng.IntN(len(alphabet))])
						ops = append(ops, op)
					}
				}
				return ops
			}
			lower := []rune("abcdefgh")

			mustApply(t, b, editRandomly(a, 30, lower)...)
			version := a.Snapshot()
			wantVersion := a.String()
			mustApply(t, b, editRandomly(a, 30, lower)...)

			// a reverts; b concurrently types characters nobody else uses.
			rev := revert(t, a, version, 3)
			mustApply(t, a, rev...)
			if a.String() != wantVersion {
				t.Fatalf("revert did not restore the version: %q, want %q", a.String(), wantVersion)
			}
			concurrent := editRandomly(b, 10, []rune("XYZ"))

			mustApply(t, a, concurrent...)
			mustApply(t, b, rev...)
			checkInvariants(t, a)
			checkInvariants(t, b)
			if a.String() != b.String() {
				t.Fatalf("diverged:\n a %q\n b %q", a.String(), b.String())
			}

			// Concurrent inserts that b did not delete itself are all there.
			var survivors []rune
			for _, op := range concurrent {
				if ins, ok := op.(Insert); ok && !b.nodes[ins.ID].deleted {
					survivors = append(survivors, ins.Char)
				}
			}
			for _, r := range survivors {
				if !strings.ContainsRune(a.String(), r) {
					t.Fatalf("concurrent %q lost: %q", r, a.String())
				}
			}
			// Without concurrent edits the result is exactly the version.
			stripped := strings.Map(func(r rune) rune {
				if strings.ContainsRune("XYZ", r) {
					return -1
				}
				return r
			}, a.String())
			if !isSubsequence(stripped, wantVersion) {
				t.Fatalf("revert result %q is not the version %q with concurrent edits", a.String(), wantVersion)
			}
		})
	}
}

// isSubsequence reports whether s can be obtained from t by deleting runes
// (b's concurrent deletes may remove characters of the version).
func isSubsequence(s, t string) bool {
	tr := []rune(t)
	i := 0
	for _, r := range s {
		for i < len(tr) && tr[i] != r {
			i++
		}
		if i == len(tr) {
			return false
		}
		i++
	}
	return true
}
