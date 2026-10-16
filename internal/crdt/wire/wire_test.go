package wire

import (
	"bytes"
	"encoding/json"
	"os"
	"testing"

	"cloute/internal/crdt"
)

func TestOpRoundTrip(t *testing.T) {
	ops := []crdt.Op{
		crdt.Insert{ID: crdt.ID{Lamport: 5, Replica: 2}, After: crdt.ID{}, Char: '👋'},
		crdt.Delete{ID: crdt.ID{Lamport: 5, Replica: 2}},
	}
	data, err := json.Marshal(EncodeOps(ops))
	if err != nil {
		t.Fatal(err)
	}
	if want := `[{"type":"insert","id":[5,2],"after":[0,0],"char":"👋"},{"type":"delete","id":[5,2]}]`; string(data) != want {
		t.Fatalf("encoded %s\nwant    %s", data, want)
	}
	var w []Op
	if err := json.Unmarshal(data, &w); err != nil {
		t.Fatal(err)
	}
	got, err := DecodeOps(w)
	if err != nil {
		t.Fatal(err)
	}
	for i := range ops {
		if got[i] != ops[i] {
			t.Errorf("op %d: got %+v, want %+v", i, got[i], ops[i])
		}
	}
}

func TestDecodeRejects(t *testing.T) {
	after := ID{0, 0}
	bad := []Op{
		{Type: "move", ID: ID{1, 1}},
		{Type: "insert", ID: ID{1, 1}, Char: "a"},
		{Type: "insert", ID: ID{1, 1}, After: &after, Char: "ab"},
		{Type: "insert", ID: ID{1, 1}, After: &after, Char: ""},
		{Type: "delete", ID: ID{MaxSafeInteger + 1, 1}},
		{Type: "delete", ID: ID{1, 1 << 32}},
	}
	for _, w := range bad {
		if _, err := DecodeOp(w); err == nil {
			t.Errorf("DecodeOp(%+v) succeeded, want error", w)
		}
	}
}

// TestSnapshotFixture pins the snapshot encoding shared with the web client:
// the TypeScript tests decode the same file.
func TestSnapshotFixture(t *testing.T) {
	data, err := os.ReadFile("../../../testdata/protocol/snapshot.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Text     string          `json:"text"`
		Snapshot json.RawMessage `json:"snapshot"`
	}
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}

	// Rebuild the document of vector 09 from its operations.
	a := crdt.ID{Lamport: 1, Replica: 1}
	b := crdt.ID{Lamport: 2, Replica: 1}
	x := crdt.ID{Lamport: 3, Replica: 2}
	d := crdt.New(0)
	for _, op := range []crdt.Op{
		crdt.Insert{ID: a, After: crdt.ID{}, Char: 'A'},
		crdt.Insert{ID: b, After: a, Char: 'B'},
		crdt.Insert{ID: x, After: b, Char: 'x'},
		crdt.Insert{ID: crdt.ID{Lamport: 4, Replica: 2}, After: x, Char: 'y'},
		crdt.Insert{ID: crdt.ID{Lamport: 3, Replica: 3}, After: b, Char: 'z'},
		crdt.Delete{ID: a},
	} {
		if err := d.Apply(op); err != nil {
			t.Fatal(err)
		}
	}
	if d.String() != fixture.Text {
		t.Fatalf("document text %q, fixture says %q", d.String(), fixture.Text)
	}

	encoded, err := json.Marshal(EncodeSnapshot(d.Snapshot()))
	if err != nil {
		t.Fatal(err)
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, fixture.Snapshot); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(encoded, compact.Bytes()) {
		t.Fatalf("encoded snapshot\n%s\nfixture\n%s", encoded, compact.Bytes())
	}

	var s Snapshot
	if err := json.Unmarshal(fixture.Snapshot, &s); err != nil {
		t.Fatal(err)
	}
	entries, err := DecodeSnapshot(s)
	if err != nil {
		t.Fatal(err)
	}
	restored, err := crdt.Restore(7, entries)
	if err != nil {
		t.Fatal(err)
	}
	if restored.String() != fixture.Text {
		t.Fatalf("restored text %q, want %q", restored.String(), fixture.Text)
	}
}
