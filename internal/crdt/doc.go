package crdt

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"
)

var (
	ErrInvalidOp  = errors.New("crdt: invalid operation")
	ErrOutOfRange = errors.New("crdt: position out of range")
)

type node struct {
	id      ID
	after   ID
	char    rune
	deleted bool
	next    *node
}

// не потокобезопасен
type Doc struct {
	replica uint32
	clock   uint64
	head    *node
	nodes   map[ID]*node
	length  int

	// ждут свою зависимость
	pending      map[ID][]Op
	pendingCount int
}

func New(replica uint32) *Doc {
	head := &node{}
	return &Doc{
		replica: replica,
		head:    head,
		nodes:   map[ID]*node{{}: head},
		pending: make(map[ID][]Op),
	}
}

func (d *Doc) String() string {
	var b strings.Builder
	for n := d.head.next; n != nil; n = n.next {
		if !n.deleted {
			b.WriteRune(n.char)
		}
	}
	return b.String()
}

func (d *Doc) Len() int {
	return d.length
}

func (d *Doc) Contains(id ID) bool {
	_, ok := d.nodes[id]
	return ok
}

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

func (d *Doc) Pending() int {
	return d.pendingCount
}

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

func (d *Doc) DeleteAt(pos int) (Delete, error) {
	if pos < 0 || pos >= d.length {
		return Delete{}, fmt.Errorf("%w: delete at %d, length %d", ErrOutOfRange, pos, d.length)
	}
	n := d.visibleAt(pos)
	d.tombstone(n)
	return Delete{ID: n.id}, nil
}

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

func (d *Doc) observe(id ID) {
	d.clock = max(d.clock, id.Lamport)
}

// пропускаем поддеревья с большим ID
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
				// такой ID уже занят
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
