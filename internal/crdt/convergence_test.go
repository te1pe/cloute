package crdt

import (
	"fmt"
	"math/rand/v2"
	"testing"
)

var alphabet = []rune("abcxyzабв👋 \n")

func TestConvergenceRandomized(t *testing.T) {
	const (
		replicas = 3
		steps    = 400
		seeds    = 200
	)
	for seed := range uint64(seeds) {
		t.Run(fmt.Sprintf("seed=%d", seed), func(t *testing.T) {
			simulate(t, seed, replicas, steps)
		})
	}
}

func simulate(t *testing.T, seed uint64, replicas, steps int) {
	rng := rand.New(rand.NewPCG(seed, 0))

	docs := make([]*Doc, replicas)
	inbox := make([][]Op, replicas)
	for i := range docs {
		docs[i] = New(uint32(i + 1))
	}
	var log []Op

	broadcast := func(from int, op Op) {
		log = append(log, op)
		for i := range inbox {
			if i != from {
				inbox[i] = append(inbox[i], op)
			}
		}
	}

	for range steps {
		r := rng.IntN(replicas)
		d := docs[r]
		switch k := rng.IntN(10); {
		case k < 5:
			op, err := d.InsertAt(rng.IntN(d.Len()+1), alphabet[rng.IntN(len(alphabet))])
			if err != nil {
				t.Fatal(err)
			}
			broadcast(r, op)
		case k < 7 && d.Len() > 0:
			op, err := d.DeleteAt(rng.IntN(d.Len()))
			if err != nil {
				t.Fatal(err)
			}
			broadcast(r, op)
		default:
			box := inbox[r]
			rng.Shuffle(len(box), func(i, j int) { box[i], box[j] = box[j], box[i] })
			n := rng.IntN(len(box) + 1)
			mustApply(t, d, box[:n]...)
			inbox[r] = box[n:]
		}
	}

	for _, d := range docs {
		all := append([]Op(nil), log...)
		rng.Shuffle(len(all), func(i, j int) { all[i], all[j] = all[j], all[i] })
		mustApply(t, d, all...)
	}

	fresh := New(uint32(replicas + 1))
	all := append([]Op(nil), log...)
	rng.Shuffle(len(all), func(i, j int) { all[i], all[j] = all[j], all[i] })
	mustApply(t, fresh, all...)
	docs = append(docs, fresh)

	want := docs[0].String()
	for i, d := range docs {
		checkInvariants(t, d)
		if d.Pending() != 0 {
			t.Fatalf("replica %d: %d ops still pending", i+1, d.Pending())
		}
		if got := d.String(); got != want {
			t.Fatalf("replica %d diverged:\n got %q\nwant %q", i+1, got, want)
		}
	}
}
