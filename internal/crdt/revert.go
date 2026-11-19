package crdt

import "fmt"

func (d *Doc) RevertTo(version []Entry, replica uint32) ([]Op, error) {
	inVersion := make(map[ID]bool, len(version))
	for _, e := range version {
		if !d.Contains(e.ID) {
			return nil, fmt.Errorf("%w: version has %v, unknown to the document", ErrInvalidOp, e.ID)
		}
		inVersion[e.ID] = !e.Deleted
	}

	var ops []Op
	for n := d.head.next; n != nil; n = n.next {
		if !n.deleted && !inVersion[n.id] {
			ops = append(ops, Delete{ID: n.id})
		}
	}

	clock := d.clock
	prev := ID{}
	for _, e := range version {
		if e.Deleted {
			continue
		}
		if !d.nodes[e.ID].deleted {
			prev = e.ID
			continue
		}
		clock++
		copyID := ID{Lamport: clock, Replica: replica}
		ops = append(ops, Insert{ID: copyID, After: prev, Char: e.Char})
		prev = copyID
	}
	return ops, nil
}
