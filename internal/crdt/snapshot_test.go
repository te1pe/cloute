package crdt

import (
	"errors"
	"fmt"
	"math/rand/v2"
	"slices"
	"testing"
)

func TestSnapshotRoundTrip(t *testing.T) {
	a := ins(1, 1, ID{}, 'a')
	b := ins(2, 1, a.ID, 'b')
	x := ins(3, 2, a.ID, 'x')
	d := New(9)
	mustApply(t, d, a, b, x, Delete{ID: a.ID})

	snap := d.Snapshot()
	want := []Entry{
		{ID: a.ID, After: ID{}, Char: 'a', Deleted: true},
		{ID: x.ID, After: a.ID, Char: 'x'},
		{ID: b.ID, After: a.ID, Char: 'b'},
	}
	if !slices.Equal(snap, want) {
		t.Fatalf("Snapshot() = %+v\nwant %+v", snap, want)
	}

	r, err := Restore(5, snap)
	if err != nil {
		t.Fatal(err)
	}
	checkInvariants(t, r)
	if r.String() != "xb" || r.Len() != 2 {
		t.Fatalf("restored text = %q (len %d), want %q", r.String(), r.Len(), "xb")
	}

	// The clock is restored too: new local IDs are above everything seen.
	op, _ := r.InsertAt(0, 'n')
	if op.ID != (ID{4, 5}) {
		t.Fatalf("first local insert after Restore got %v, want {4 5}", op.ID)
	}
}

func TestRestoreRejectsMalformed(t *testing.T) {
	a := Entry{ID: ID{1, 1}, After: ID{}, Char: 'a'}
	tests := []struct {
		name    string
		entries []Entry
	}{
		{"zero ID", []Entry{{ID: ID{}, After: ID{}, Char: 'a'}}},
		{"Lamport not above After", []Entry{a, {ID: ID{1, 2}, After: a.ID, Char: 'b'}}},
		{"invalid rune", []Entry{{ID: ID{1, 1}, After: ID{}, Char: 0xD800}}},
		{"duplicate", []Entry{a, a}},
		{"After not earlier in the list", []Entry{{ID: ID{2, 1}, After: a.ID, Char: 'b'}, a}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := Restore(1, tt.entries); !errors.Is(err, ErrInvalidOp) {
				t.Fatalf("Restore: err = %v, want ErrInvalidOp", err)
			}
		})
	}
}

// TestRestoredReplicaBehavesLikeOriginal checks that a restored replica is
// indistinguishable from the original: after the same further operations,
// in different orders, both hold the same snapshot.
func TestRestoredReplicaBehavesLikeOriginal(t *testing.T) {
	for seed := range uint64(100) {
		t.Run(fmt.Sprintf("seed=%d", seed), func(t *testing.T) {
			rng := rand.New(rand.NewPCG(seed, 1))
			editors := []*Doc{New(1), New(2)}
			var history, future []Op

			edit := func(log *[]Op) {
				e := editors[rng.IntN(len(editors))]
				var op Op
				if e.Len() > 0 && rng.IntN(3) == 0 {
					op, _ = e.DeleteAt(rng.IntN(e.Len()))
				} else {
					op, _ = e.InsertAt(rng.IntN(e.Len()+1), alphabet[rng.IntN(len(alphabet))])
				}
				*log = append(*log, op)
				for _, other := range editors {
					if other != e {
						mustApply(t, other, op)
					}
				}
			}
			for range 150 {
				edit(&history)
			}
			for range 150 {
				edit(&future)
			}

			original := New(3)
			mustApply(t, original, history...)
			restored, err := Restore(3, original.Snapshot())
			if err != nil {
				t.Fatal(err)
			}

			mustApply(t, original, future...)
			shuffled := slices.Clone(future)
			rng.Shuffle(len(shuffled), func(i, j int) { shuffled[i], shuffled[j] = shuffled[j], shuffled[i] })
			mustApply(t, restored, shuffled...)

			checkInvariants(t, restored)
			if !slices.Equal(original.Snapshot(), restored.Snapshot()) {
				t.Fatalf("restored replica diverged:\n got %q\nwant %q", restored.String(), original.String())
			}
		})
	}
}
