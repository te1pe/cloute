package crdt

import (
	"encoding/json"
	"fmt"
	"math"
	"math/rand/v2"
	"os"
	"path/filepath"
	"testing"
	"unicode/utf8"
)

// The shared test vectors are also run by the TypeScript port; the format is
// described in testdata/crdt/README.md.
const vectorsDir = "../../testdata/crdt"

type vector struct {
	Name        string     `json:"name"`
	Description string     `json:"description"`
	Ops         []vectorOp `json:"ops"`
	Expected    string     `json:"expected"`
}

type vectorOp struct {
	Type  string     `json:"type"`
	ID    [2]uint64  `json:"id"`
	After *[2]uint64 `json:"after"`
	Char  string     `json:"char"`
}

func toID(v [2]uint64) (ID, error) {
	if v[1] > math.MaxUint32 {
		return ID{}, fmt.Errorf("replica %d does not fit in uint32", v[1])
	}
	return ID{Lamport: v[0], Replica: uint32(v[1])}, nil
}

func (v vectorOp) toOp() (Op, error) {
	id, err := toID(v.ID)
	if err != nil {
		return nil, err
	}
	switch v.Type {
	case "insert":
		if v.After == nil {
			return nil, fmt.Errorf("insert %v: missing after", v.ID)
		}
		after, err := toID(*v.After)
		if err != nil {
			return nil, err
		}
		r, size := utf8.DecodeRuneInString(v.Char)
		if size == 0 || size != len(v.Char) {
			return nil, fmt.Errorf("insert %v: char %q must be exactly one code point", v.ID, v.Char)
		}
		return Insert{ID: id, After: after, Char: r}, nil
	case "delete":
		return Delete{ID: id}, nil
	default:
		return nil, fmt.Errorf("unknown op type %q", v.Type)
	}
}

func TestVectors(t *testing.T) {
	files, err := filepath.Glob(filepath.Join(vectorsDir, "*.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatalf("no test vectors in %s", vectorsDir)
	}

	for _, file := range files {
		t.Run(filepath.Base(file), func(t *testing.T) {
			data, err := os.ReadFile(file)
			if err != nil {
				t.Fatal(err)
			}
			var v vector
			if err := json.Unmarshal(data, &v); err != nil {
				t.Fatal(err)
			}
			ops := make([]Op, len(v.Ops))
			for i, raw := range v.Ops {
				if ops[i], err = raw.toOp(); err != nil {
					t.Fatalf("op %d: %v", i, err)
				}
			}

			for _, order := range deliveryOrders(len(ops)) {
				d := New(0)
				for _, i := range order {
					mustApply(t, d, ops[i])
				}
				if got := d.String(); got != v.Expected || d.Pending() != 0 {
					t.Fatalf("order %v: text = %q, pending = %d; want %q, 0 pending", order, got, d.Pending(), v.Expected)
				}
				// Delivering everything again must not change anything.
				for _, i := range order {
					mustApply(t, d, ops[i])
				}
				if got := d.String(); got != v.Expected {
					t.Fatalf("order %v, redelivered: text = %q, want %q", order, got, v.Expected)
				}
			}
		})
	}
}

// deliveryOrders returns every permutation of n operations for small n,
// otherwise the listed order, the reverse order and random permutations
// with a fixed seed.
func deliveryOrders(n int) [][]int {
	const maxExhaustive = 7 // 7! = 5040 orders
	if n <= maxExhaustive {
		return permutations(n)
	}

	identity := make([]int, n)
	reverse := make([]int, n)
	for i := range n {
		identity[i] = i
		reverse[i] = n - 1 - i
	}
	orders := [][]int{identity, reverse}
	rng := rand.New(rand.NewPCG(1, 0))
	for range 1000 {
		orders = append(orders, rng.Perm(n))
	}
	return orders
}

// permutations returns all permutations of 0..n-1 (Heap's algorithm).
func permutations(n int) [][]int {
	p := make([]int, n)
	for i := range p {
		p[i] = i
	}
	var out [][]int
	var generate func(k int)
	generate = func(k int) {
		if k <= 1 {
			out = append(out, append([]int(nil), p...))
			return
		}
		for i := range k - 1 {
			generate(k - 1)
			if k%2 == 0 {
				p[i], p[k-1] = p[k-1], p[i]
			} else {
				p[0], p[k-1] = p[k-1], p[0]
			}
		}
		generate(k - 1)
	}
	generate(n)
	return out
}

func TestPermutations(t *testing.T) {
	for n, want := range []int{1, 1, 2, 6, 24, 120} {
		perms := permutations(n)
		seen := make(map[string]bool)
		for _, p := range perms {
			seen[fmt.Sprint(p)] = true
		}
		if len(perms) != want || len(seen) != want {
			t.Errorf("permutations(%d): %d results, %d distinct; want %d", n, len(perms), len(seen), want)
		}
	}
}
