package crdt

import "fmt"

// RevertTo returns ordinary operations that make the visible text of d equal
// to the visible text of version, a snapshot of this same document taken
// earlier. History is not rewritten: the operations are applied, stored and
// merged like any other, so edits made concurrently with the revert survive.
//
// It changes as little as possible, matching characters by ID:
//   - visible now but not in the version: deleted;
//   - visible in the version but deleted now: inserted again as a copy (RGA
//     cannot undelete a tombstone), right after the character that precedes
//     it in the version;
//   - everything else is left alone, so concurrent edits keep the anchors
//     they were made against.
//
// New characters get IDs of the given replica with Lamport timestamps above
// everything d has seen. d itself is not modified.
func (d *Doc) RevertTo(version []Entry, replica uint32) ([]Op, error) {
	inVersion := make(map[ID]bool, len(version)) // ID -> visible in the version
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
	prev := ID{} // the last character that will be visible, in version order
	for _, e := range version {
		if e.Deleted {
			continue
		}
		if !d.nodes[e.ID].deleted {
			prev = e.ID // kept as it is
			continue
		}
		clock++
		copyID := ID{Lamport: clock, Replica: replica}
		// The greatest ID so far: integration puts it right after prev.
		ops = append(ops, Insert{ID: copyID, After: prev, Char: e.Char})
		prev = copyID
	}
	return ops, nil
}
