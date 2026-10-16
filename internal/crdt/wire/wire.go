// Package wire is the JSON encoding of CRDT operations and snapshots shared
// by the WebSocket protocol and the database. The format is specified in
// api/protocol.md and matches testdata/crdt.
package wire

import (
	"errors"
	"fmt"
	"math"
	"unicode/utf8"

	"cloute/internal/crdt"
)

// MaxSafeInteger is the largest integer a JavaScript number holds exactly;
// Lamport timestamps above it are rejected so that web clients stay exact.
const MaxSafeInteger = 1<<53 - 1

// ID is [lamport, replica].
type ID [2]uint64

// Op is an insert or a delete.
type Op struct {
	Type  string `json:"type"`
	ID    ID     `json:"id"`
	After *ID    `json:"after,omitempty"`
	Char  string `json:"char,omitempty"`
}

// Snapshot is the document state: all characters, including tombstones,
// in document order.
type Snapshot struct {
	Chars []Entry `json:"chars"`
}

// Entry is one character of a snapshot.
type Entry struct {
	ID      ID     `json:"id"`
	After   ID     `json:"after"`
	Char    string `json:"char"`
	Deleted bool   `json:"deleted,omitempty"`
}

func EncodeOps(ops []crdt.Op) []Op {
	out := make([]Op, 0, len(ops))
	for _, op := range ops {
		out = append(out, EncodeOp(op))
	}
	return out
}

func EncodeOp(op crdt.Op) Op {
	switch op := op.(type) {
	case crdt.Insert:
		after := encodeID(op.After)
		return Op{Type: "insert", ID: encodeID(op.ID), After: &after, Char: string(op.Char)}
	case crdt.Delete:
		return Op{Type: "delete", ID: encodeID(op.ID)}
	default:
		panic(fmt.Sprintf("wire: unknown op type %T", op))
	}
}

func DecodeOps(in []Op) ([]crdt.Op, error) {
	out := make([]crdt.Op, 0, len(in))
	for i, w := range in {
		op, err := DecodeOp(w)
		if err != nil {
			return nil, fmt.Errorf("op %d: %w", i, err)
		}
		out = append(out, op)
	}
	return out, nil
}

func DecodeOp(w Op) (crdt.Op, error) {
	id, err := decodeID(w.ID)
	if err != nil {
		return nil, err
	}
	switch w.Type {
	case "insert":
		if w.After == nil {
			return nil, errors.New("insert without after")
		}
		after, err := decodeID(*w.After)
		if err != nil {
			return nil, err
		}
		r, err := decodeChar(w.Char)
		if err != nil {
			return nil, err
		}
		return crdt.Insert{ID: id, After: after, Char: r}, nil
	case "delete":
		return crdt.Delete{ID: id}, nil
	default:
		return nil, fmt.Errorf("unknown op type %q", w.Type)
	}
}

func EncodeSnapshot(entries []crdt.Entry) Snapshot {
	chars := make([]Entry, 0, len(entries))
	for _, e := range entries {
		chars = append(chars, Entry{ID: encodeID(e.ID), After: encodeID(e.After), Char: string(e.Char), Deleted: e.Deleted})
	}
	return Snapshot{Chars: chars}
}

func DecodeSnapshot(s Snapshot) ([]crdt.Entry, error) {
	out := make([]crdt.Entry, 0, len(s.Chars))
	for i, w := range s.Chars {
		id, err := decodeID(w.ID)
		if err != nil {
			return nil, fmt.Errorf("char %d: %w", i, err)
		}
		after, err := decodeID(w.After)
		if err != nil {
			return nil, fmt.Errorf("char %d: %w", i, err)
		}
		r, err := decodeChar(w.Char)
		if err != nil {
			return nil, fmt.Errorf("char %d: %w", i, err)
		}
		out = append(out, crdt.Entry{ID: id, After: after, Char: r, Deleted: w.Deleted})
	}
	return out, nil
}

// EncodeID encodes one ID (cursors and comment anchors use it too).
func EncodeID(id crdt.ID) ID { return encodeID(id) }

// DecodeID decodes and checks one ID.
func DecodeID(w ID) (crdt.ID, error) { return decodeID(w) }

func encodeID(id crdt.ID) ID {
	return ID{id.Lamport, uint64(id.Replica)}
}

func decodeID(w ID) (crdt.ID, error) {
	if w[0] > MaxSafeInteger {
		return crdt.ID{}, fmt.Errorf("lamport %d exceeds 2^53-1", w[0])
	}
	if w[1] > math.MaxUint32 {
		return crdt.ID{}, fmt.Errorf("replica %d exceeds 2^32-1", w[1])
	}
	return crdt.ID{Lamport: w[0], Replica: uint32(w[1])}, nil
}

// decodeChar accepts exactly one code point. encoding/json has already
// replaced invalid UTF-8 and lone surrogates with U+FFFD.
func decodeChar(s string) (rune, error) {
	r, size := utf8.DecodeRuneInString(s)
	if size == 0 || size != len(s) {
		return 0, fmt.Errorf("char %q must be exactly one Unicode code point", s)
	}
	return r, nil
}
