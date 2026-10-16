package crdt

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"
)

var (
	// ErrInvalidOp is returned for operations that no correct replica can produce.
	ErrInvalidOp = errors.New("crdt: invalid operation")
	// ErrOutOfRange is returned for local edits at a position outside the text.
	ErrOutOfRange = errors.New("crdt: position out of range")
)

// node is one character of the sequence, visible or tombstoned.
type node struct {
	id      ID
	after   ID
	char    rune
	deleted bool
	next    *node
}

// Doc is one replica of a text document.
//
// Characters are kept in a singly linked list in document order, including
// tombstones, plus an index from ID to node. Doc is not safe for concurrent
// use: it is meant to be owned by a single goroutine.
type Doc struct {
	replica uint32
	clock   uint64 // greatest Lamport timestamp seen so far
	head    *node  // virtual element with the zero ID, never visible
	nodes   map[ID]*node
	length  int // number of visible characters

	// pending holds operations whose dependency (the After of an insert or
	// the target of a delete) is not known yet, keyed by that missing ID.
	pending      map[ID][]Op
	pendingCount int
}

// New returns an empty document replica that creates IDs with the given
// replica number.
func New(replica uint32) *Doc {
	head := &node{}
	return &Doc{
		replica: replica,
		head:    head,
		nodes:   map[ID]*node{{}: head},
		pending: make(map[ID][]Op),
	}
}

// String returns the visible text.
func (d *Doc) String() string {
	var b strings.Builder
	for n := d.head.next; n != nil; n = n.next {
		if !n.deleted {
			b.WriteRune(n.char)
		}
	}
	return b.String()
}

// Len returns the number of visible characters.
func (d *Doc) Len() int {
	return d.length
}

// Contains reports whether the character id has been applied, visible or
// deleted. The zero ID (the head) is always present.
func (d *Doc) Contains(id ID) bool {
	_, ok := d.nodes[id]
	return ok
}

// InOrder reports whether the characters a and b are both known and a does
// not come after b in document order (tombstones included). The order of
// two characters never changes, so a range [a, b] stays a range.
func (d *Doc) InOrder(a, b ID) bool {
	if !d.Contains(a) || !d.Contains(b) {
		return false
	}
	for n := d.head.next; n != nil; n = n.next {
		switch n.id {
		case a:
			return true
		case b:
			return false
		}
	}
	return false
}

// Applied reports whether op has already taken effect here: the insert's
// character exists or the delete's target is already deleted. Applying it
// again changes nothing. (An insert whose ID exists with different content
// is reported as applied too; Apply rejects it.)
func (d *Doc) Applied(op Op) bool {
	switch op := op.(type) {
	case Insert:
		return d.Contains(op.ID)
	case Delete:
		n, ok := d.nodes[op.ID]
		return ok && n.deleted
	default:
		return false
	}
}

// Pending returns the number of buffered operations waiting for a dependency.
func (d *Doc) Pending() int {
	return d.pendingCount
}

// InsertAt inserts char before the visible character at pos (pos == Len()
// appends) and returns the operation to send to other replicas.
func (d *Doc) InsertAt(pos int, char rune) (Insert, error) {
	if pos < 0 || pos > d.length {
		return Insert{}, fmt.Errorf("%w: insert at %d, length %d", ErrOutOfRange, pos, d.length)
	}
	if !utf8.ValidRune(char) {
		return Insert{}, fmt.Errorf("%w: invalid rune %U", ErrInvalidOp, char)
	}

	after := d.head
	if pos > 0 {
		after = d.visibleAt(pos - 1)
	}
	d.clock++
	op := Insert{ID: ID{Lamport: d.clock, Replica: d.replica}, After: after.id, Char: char}
	d.integrate(op)
	d.release(op.ID)
	return op, nil
}

// DeleteAt deletes the visible character at pos and returns the operation
// to send to other replicas.
func (d *Doc) DeleteAt(pos int) (Delete, error) {
	if pos < 0 || pos >= d.length {
		return Delete{}, fmt.Errorf("%w: delete at %d, length %d", ErrOutOfRange, pos, d.length)
	}
	n := d.visibleAt(pos)
	d.tombstone(n)
	return Delete{ID: n.id}, nil
}

// Apply applies an operation received from another replica.
//
// Operations may arrive in any order and more than once. An operation whose
// dependency is unknown is buffered and applied as soon as the dependency
// arrives; a repeated operation changes nothing.
func (d *Doc) Apply(op Op) error {
	switch op := op.(type) {
	case Insert:
		return d.applyInsert(op)
	case Delete:
		return d.applyDelete(op)
	default:
		return fmt.Errorf("%w: unknown operation type %T", ErrInvalidOp, op)
	}
}

func (d *Doc) applyInsert(op Insert) error {
	if op.ID.Lamport == 0 {
		return fmt.Errorf("%w: insert with zero Lamport timestamp", ErrInvalidOp)
	}
	if op.ID.Lamport <= op.After.Lamport {
		return fmt.Errorf("%w: insert %v must have a greater Lamport timestamp than %v", ErrInvalidOp, op.ID, op.After)
	}
	if !utf8.ValidRune(op.Char) {
		return fmt.Errorf("%w: invalid rune %U", ErrInvalidOp, op.Char)
	}
	if n, ok := d.nodes[op.ID]; ok {
		if n.after != op.After || n.char != op.Char {
			return fmt.Errorf("%w: insert %v conflicts with an existing character", ErrInvalidOp, op.ID)
		}
		return nil
	}

	d.observe(op.ID)
	if _, ok := d.nodes[op.After]; !ok {
		d.buffer(op.After, op)
		return nil
	}
	d.integrate(op)
	d.release(op.ID)
	return nil
}

func (d *Doc) applyDelete(op Delete) error {
	if op.ID.Lamport == 0 {
		return fmt.Errorf("%w: delete with zero Lamport timestamp", ErrInvalidOp)
	}

	d.observe(op.ID)
	n, ok := d.nodes[op.ID]
	if !ok {
		d.buffer(op.ID, op)
		return nil
	}
	d.tombstone(n)
	return nil
}

// observe advances the Lamport clock so that IDs created locally later are
// greater than every ID this replica has seen.
func (d *Doc) observe(id ID) {
	d.clock = max(d.clock, id.Lamport)
}

// integrate links a new character into the list. Its After must be known.
//
// Starting right after After, it skips every element with a greater ID.
// Since a character's Lamport timestamp is always greater than its After's,
// this skips exactly the concurrent siblings with greater IDs together with
// everything inserted after them, which gives descending ID order among
// siblings on every replica regardless of arrival order.
func (d *Doc) integrate(op Insert) {
	prev := d.nodes[op.After]
	for prev.next != nil && op.ID.Less(prev.next.id) {
		prev = prev.next
	}
	n := &node{id: op.ID, after: op.After, char: op.Char, next: prev.next}
	prev.next = n
	d.nodes[op.ID] = n
	d.length++
}

func (d *Doc) tombstone(n *node) {
	if n.deleted {
		return
	}
	n.deleted = true
	d.length--
}

func (d *Doc) buffer(missing ID, op Op) {
	if slices.Contains(d.pending[missing], op) {
		return
	}
	d.pending[missing] = append(d.pending[missing], op)
	d.pendingCount++
}

// release applies buffered operations that were waiting for id, and then,
// transitively, those waiting for the characters they inserted.
func (d *Doc) release(id ID) {
	ready := []ID{id}
	for len(ready) > 0 {
		id := ready[len(ready)-1]
		ready = ready[:len(ready)-1]

		waiting := d.pending[id]
		delete(d.pending, id)
		d.pendingCount -= len(waiting)

		for _, op := range waiting {
			switch op := op.(type) {
			case Insert:
				// A different insert with the same ID may have won the race;
				// IDs are unique among correct replicas, so keep the first.
				if _, ok := d.nodes[op.ID]; ok {
					continue
				}
				d.integrate(op)
				ready = append(ready, op.ID)
			case Delete:
				d.tombstone(d.nodes[op.ID])
			}
		}
	}
}

// visibleAt returns the visible character at pos; pos must be in range.
func (d *Doc) visibleAt(pos int) *node {
	for n := d.head.next; n != nil; n = n.next {
		if n.deleted {
			continue
		}
		if pos == 0 {
			return n
		}
		pos--
	}
	panic("crdt: visibleAt: position out of range")
}
