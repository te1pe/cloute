package crdt

import (
	"fmt"
	"unicode/utf8"
)

// Entry is one character of a snapshot, visible or tombstoned.
type Entry struct {
	ID      ID
	After   ID
	Char    rune
	Deleted bool
}

// Snapshot returns every applied character, including tombstones, in
// document order. Buffered operations are not part of the snapshot.
func (d *Doc) Snapshot() []Entry {
	entries := make([]Entry, 0, len(d.nodes)-1)
	for n := d.head.next; n != nil; n = n.next {
		entries = append(entries, Entry{ID: n.id, After: n.after, Char: n.char, Deleted: n.deleted})
	}
	return entries
}

// Restore builds a replica from a snapshot made by Snapshot.
//
// The list is linked in the given order, in O(n), without re-running the
// placement of each character. Restore checks that every entry is well
// formed and refers to a character earlier in the list (true for any
// document order: a character always follows the one it was inserted after),
// but it trusts that the order itself is the one Snapshot produced.
func Restore(replica uint32, entries []Entry) (*Doc, error) {
	d := New(replica)
	prev := d.head
	for i, e := range entries {
		switch {
		case e.ID.Lamport == 0:
			return nil, fmt.Errorf("%w: snapshot entry %d has zero Lamport timestamp", ErrInvalidOp, i)
		case e.ID.Lamport <= e.After.Lamport:
			return nil, fmt.Errorf("%w: snapshot entry %d: %v must have a greater Lamport timestamp than %v", ErrInvalidOp, i, e.ID, e.After)
		case !utf8.ValidRune(e.Char):
			return nil, fmt.Errorf("%w: snapshot entry %d: invalid rune %U", ErrInvalidOp, i, e.Char)
		case d.Contains(e.ID):
			return nil, fmt.Errorf("%w: snapshot entry %d: duplicate %v", ErrInvalidOp, i, e.ID)
		case !d.Contains(e.After):
			return nil, fmt.Errorf("%w: snapshot entry %d: %v refers to %v that is not before it", ErrInvalidOp, i, e.ID, e.After)
		}

		n := &node{id: e.ID, after: e.After, char: e.Char, deleted: e.Deleted}
		prev.next = n
		prev = n
		d.nodes[e.ID] = n
		if !e.Deleted {
			d.length++
		}
		d.observe(e.ID)
	}
	return d, nil
}
