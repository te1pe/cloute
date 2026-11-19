package crdt

type ID struct {
	Lamport uint64
	Replica uint32
}

// меньший lamport раньше, при равенстве — меньшая реплика
func (a ID) Less(b ID) bool {
	if a.Lamport != b.Lamport {
		return a.Lamport < b.Lamport
	}
	return a.Replica < b.Replica
}

type Op interface {
	isOp()
}

type Insert struct {
	ID    ID
	After ID
	Char  rune
}

// удаление = tombstone
type Delete struct {
	ID ID
}

func (Insert) isOp() {}
func (Delete) isOp() {}
