package crdt

import (
	"fmt"
	"unicode/utf8"
)

type Entry struct {
	ID      ID
	After   ID
	Char    rune
	Deleted bool
}

func (d *Doc) Snapshot() []Entry {
	entries := make([]Entry, 0, len(d.nodes)-1)
	for n := d.head.next; n != nil; n = n.next {
		entries = append(entries, Entry{ID: n.id, After: n.after, Char: n.char, Deleted: n.deleted})
	}
	return entries
}

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
