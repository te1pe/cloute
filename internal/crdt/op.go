// Package crdt implements RGA (Replicated Growable Array), a sequence CRDT
// used to merge concurrent edits of plain text without conflicts.
//
// Every character has a globally unique ID. An insert names the character it
// is placed after; a delete marks a character as removed (a tombstone) but
// keeps it in the sequence so that later inserts can still refer to it.
// Replicas that applied the same set of operations, in any order, hold the
// same text.
package crdt

// ID uniquely identifies a character across all replicas of a document.
//
// The zero ID is reserved for the virtual head of the sequence: an insert
// with After equal to ID{} places the character at the very beginning.
// Real characters always have Lamport >= 1.
type ID struct {
	Lamport uint64
	Replica uint32
}

// Less reports whether a precedes b in ID order: lower Lamport first,
// ties broken by lower Replica. Concurrent inserts after the same character
// are placed in descending ID order, so the greatest ID comes first.
func (a ID) Less(b ID) bool {
	if a.Lamport != b.Lamport {
		return a.Lamport < b.Lamport
	}
	return a.Replica < b.Replica
}

// Op is an operation on a document: either Insert or Delete.
type Op interface {
	isOp()
}

// Insert places Char immediately after the character After.
// ID.Lamport must be greater than After.Lamport.
type Insert struct {
	ID    ID
	After ID
	Char  rune
}

// Delete marks the character ID as removed.
type Delete struct {
	ID ID
}

func (Insert) isOp() {}
func (Delete) isOp() {}
