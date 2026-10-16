// Package doc runs open documents: one Hub goroutine per document owns its
// CRDT state, orders accepted operations, persists them and fans them out
// to clients.
package doc

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"cloute/internal/auth"
	"cloute/internal/crdt"
)

var (
	// ErrHubClosed means the hub has stopped: the server is shutting down
	// or the document was unloaded.
	ErrHubClosed = errors.New("doc: hub closed")
	// ErrSlowClient means the client's outgoing queue overflowed.
	ErrSlowClient = errors.New("doc: client too slow")
	// ErrInvalidOp means the client sent an operation the hub refused.
	ErrInvalidOp = errors.New("doc: invalid operation")
	// ErrStorage means the hub stopped because the store failed.
	ErrStorage = errors.New("doc: storage failure")
	// ErrUnknownReplica means a client asked to resume a replica number
	// that was never issued to its user in the document.
	ErrUnknownReplica = errors.New("doc: unknown replica")
	// ErrForbidden means the client's role does not allow editing.
	ErrForbidden = errors.New("doc: role does not allow editing")
	// ErrAccessChanged means the user's role in the document changed (an
	// access link was revoked); the client reconnects and is checked again.
	ErrAccessChanged = errors.New("doc: access changed")
	// ErrReplaced means a newer connection resumed the client's replica;
	// the old connection is most likely dead already.
	ErrReplaced = errors.New("doc: replaced by a newer connection")
)

// JoinRequest describes a client that attaches to a hub.
type JoinRequest struct {
	// UserID is the authenticated user; replicas are issued per user.
	UserID int64
	// UserName is shown to others next to the user's cursor and comments.
	UserName string
	// Role is the user's role in the document, checked for every batch of
	// operations. It is fixed for the lifetime of the connection.
	Role auth.Role
	// Replica resumes a replica number issued earlier, so that operations
	// created offline keep their IDs. 0 asks for a new replica.
	Replica uint32
	// LastSeq is the last sequence number the client has seen (0: none).
	// A client that is not too far behind gets only the operations after
	// it instead of a snapshot.
	LastSeq uint64
	// Pending are the client's operations not acknowledged yet. They are
	// applied right after the Welcome and acknowledged with one Ack.
	Pending []crdt.Op
}

// Messages a hub sends to a client, in the order the client must see them.
type (
	// Welcome is always the first message: the client's replica number and
	// the document as of Seq, either as a full Snapshot or, if CatchUp is
	// set, as the Ops after the client's JoinRequest.LastSeq.
	Welcome struct {
		Replica  uint32
		Role     auth.Role
		Seq      uint64
		CatchUp  bool
		Snapshot []crdt.Entry
		Ops      []crdt.Op
	}
	// Ack confirms that the client's batch (or its JoinRequest.Pending) was
	// accepted and stored; Seq is the hub's sequence number after it. Every
	// batch gets exactly one Ack, even if all its operations were already
	// applied before (resent after a lost Ack).
	Ack struct {
		Seq uint64
	}
	// Update carries operations accepted from another client; Seq is the
	// sequence number of the last of them.
	Update struct {
		Seq uint64
		Ops []crdt.Op
		// Received is when the server got the operations; the transport
		// measures the broadcast latency from it.
		Received time.Time
	}
	// Presence is another client's cursor; a nil Cursor means the client
	// left. Presence is not stored and has no sequence number.
	Presence struct {
		Replica  uint32
		UserID   int64
		UserName string
		Cursor   *Cursor
	}
	// Comments are all unresolved comments, sent after the Welcome if
	// there are any.
	Comments struct {
		List []Comment
	}
	// CommentChanged is a new or a resolved comment.
	CommentChanged struct {
		Comment Comment
	}
)

// Cursor is a selection; each end is the position right after a character
// (the zero ID: the start of the text). Anchoring to IDs instead of offsets
// keeps a cursor where it is while others edit before it.
type Cursor struct {
	Anchor, Head crdt.ID
}

// Comment body limits, in characters.
const maxCommentBody = 2000

// Config tunes hubs.
type Config struct {
	QueueSize     int           // outgoing messages per client before it is dropped
	IdleTimeout   time.Duration // a hub without clients unloads after this
	SnapshotEvery uint64        // operations between periodic snapshots
	MaxCatchUp    uint64        // a reconnecting client further behind gets a snapshot
	StoreTimeout  time.Duration // limit for one storage call
}

// Client is one connection attached to a hub.
//
// The hub never blocks on a client: it puts messages into a bounded queue
// and drops the client if the queue is full.
type Client struct {
	replica  uint32
	userID   int64
	userName string
	role     auth.Role
	cursor   *Cursor // owned by the hub goroutine
	out      chan any
	err      error // why the hub closed out; written before close(out)
}

// Replica returns the replica number assigned to the client.
func (c *Client) Replica() uint32 {
	return c.replica
}

// Messages returns the client's outgoing queue: Welcome, then Ack and
// Update values. The hub closes it when it detaches the client.
func (c *Client) Messages() <-chan any {
	return c.out
}

// Err reports why Messages was closed: nil after Leave, otherwise
// ErrSlowClient, ErrHubClosed, ErrReplaced, or an error wrapping
// ErrForbidden, ErrInvalidOp or ErrStorage. It must be called only after Messages is
// closed.
func (c *Client) Err() error {
	return c.err
}

type joinRequest struct {
	JoinRequest
	reply chan joinResult
}

type joinResult struct {
	client *Client
	err    error
}

type versionRequest struct {
	userID int64
	name   string
	reply  chan versionResult
}

type versionResult struct {
	version Version
	err     error
}

type restoreRequest struct {
	userID    int64
	versionID int64
	reply     chan restoreResult
}

type restoreResult struct {
	seq uint64
	err error
}

// submission is one message of a client to its hub; exactly one of the
// fields after client is set.
type submission struct {
	client   *Client
	received time.Time
	ops      []crdt.Op
	cursor   *Cursor
	comment  *newComment
	resolve  int64
}

type newComment struct {
	from, to crdt.ID
	body     string
}

// Hub owns the state of one document. All state is accessed only by the
// goroutine running run; other goroutines talk to it through channels.
type Hub struct {
	id     string
	store  Store
	cfg    Config
	logger *slog.Logger

	join     chan joinRequest
	versions chan versionRequest
	access   chan []int64
	restores chan restoreRequest
	leave    chan *Client
	submit   chan submission
	done     chan struct{}
	err      error // why the hub stopped (nil: normally); written before close(done)

	doc         *crdt.Doc
	seq         uint64 // sequence number of the last stored operation
	snapshotSeq uint64 // sequence number of the last stored snapshot
	clients     map[*Client]struct{}
	comments    map[int64]Comment // unresolved comments
	departed    []uint32          // replicas whose cursors must be removed for the others
}

func newHub(id string, store Store, cfg Config, logger *slog.Logger) *Hub {
	return &Hub{
		id:       id,
		store:    store,
		cfg:      cfg,
		logger:   logger.With("doc", id),
		join:     make(chan joinRequest),
		versions: make(chan versionRequest),
		access:   make(chan []int64),
		restores: make(chan restoreRequest),
		leave:    make(chan *Client),
		submit:   make(chan submission),
		done:     make(chan struct{}),
		clients:  make(map[*Client]struct{}),
	}
}

// Join attaches a new client. Its queue already holds the Welcome message
// and, if req has pending operations, their Ack follows. It fails with
// ErrHubClosed if the hub has stopped normally and with ErrUnknownReplica
// if req resumes a replica that was never issued.
func (h *Hub) Join(ctx context.Context, req JoinRequest) (*Client, error) {
	reply := make(chan joinResult, 1)
	select {
	case h.join <- joinRequest{JoinRequest: req, reply: reply}:
		r := <-reply
		return r.client, r.err
	case <-h.done:
		return nil, h.stopErr()
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// Leave detaches the client and closes its queue. It is a no-op if the hub
// has already dropped the client.
func (h *Hub) Leave(c *Client) {
	select {
	case h.leave <- c:
	case <-h.done:
	}
}

// Submit hands the client's operations to the hub. The result arrives
// asynchronously in the client's queue: an Ack, or the queue is closed
// with an error wrapping ErrInvalidOp.
func (h *Hub) Submit(ctx context.Context, c *Client, ops []crdt.Op) error {
	return h.post(ctx, submission{client: c, ops: ops})
}

// SetCursor shares the client's cursor with everyone else in the document.
func (h *Hub) SetCursor(ctx context.Context, c *Client, cursor Cursor) error {
	return h.post(ctx, submission{client: c, cursor: &cursor})
}

// AddComment adds a comment on the characters from..to. Everyone,
// including the author, receives it as CommentChanged.
func (h *Hub) AddComment(ctx context.Context, c *Client, from, to crdt.ID, body string) error {
	return h.post(ctx, submission{client: c, comment: &newComment{from: from, to: to, body: body}})
}

// ResolveComment closes a comment; its author and editors may.
func (h *Hub) ResolveComment(ctx context.Context, c *Client, id int64) error {
	if id <= 0 {
		return nil
	}
	return h.post(ctx, submission{client: c, resolve: id})
}

func (h *Hub) post(ctx context.Context, s submission) error {
	s.received = time.Now()
	select {
	case h.submit <- s:
		return nil
	case <-h.done:
		return h.stopErr()
	case <-ctx.Done():
		return ctx.Err()
	}
}

// AccessChanged disconnects the users' clients: their roles were read when
// they connected and may no longer hold. The clients reconnect at once and
// get their current role, or a refusal.
func (h *Hub) AccessChanged(ctx context.Context, userIDs []int64) error {
	select {
	case h.access <- userIDs:
		return nil
	case <-h.done:
		return nil // stopped: nobody to disconnect
	case <-ctx.Done():
		return ctx.Err()
	}
}

// SaveVersion stores the current state of the document as a named version.
func (h *Hub) SaveVersion(ctx context.Context, userID int64, name string) (Version, error) {
	reply := make(chan versionResult, 1)
	select {
	case h.versions <- versionRequest{userID: userID, name: name, reply: reply}:
		r := <-reply
		return r.version, r.err
	case <-h.done:
		return Version{}, h.stopErr()
	case <-ctx.Done():
		return Version{}, ctx.Err()
	}
}

// Restore makes the document's text that of the version, by appending
// ordinary operations on behalf of the user (see crdt.Doc.RevertTo). It
// returns the sequence number after them.
func (h *Hub) Restore(ctx context.Context, userID, versionID int64) (uint64, error) {
	reply := make(chan restoreResult, 1)
	select {
	case h.restores <- restoreRequest{userID: userID, versionID: versionID, reply: reply}:
		r := <-reply
		return r.seq, r.err
	case <-h.done:
		return 0, h.stopErr()
	case <-ctx.Done():
		return 0, ctx.Err()
	}
}

func (h *Hub) stopErr() error {
	if h.err != nil {
		return h.err
	}
	return ErrHubClosed
}

// run loads the document and serves it until ctx is cancelled, the hub
// stays without clients for IdleTimeout, or the store fails. onStop runs
// before done is closed, so that a caller who sees ErrHubClosed and asks
// the registry again gets a new hub.
func (h *Hub) run(ctx context.Context, onStop func()) {
	err := h.load(ctx)
	if err == nil {
		err = h.loop(ctx)
	}

	reason := ErrHubClosed
	if err != nil {
		// The in-memory state may be ahead of the store: discard it and do
		// not snapshot it. Clients reconnect to a hub loaded from the store.
		h.logger.Error("hub stopped on storage failure", "err", err)
		h.err = fmt.Errorf("%w: %w", ErrStorage, err)
		reason = h.err
	} else {
		h.saveSnapshot(ctx)
	}
	for c := range h.clients {
		h.drop(c, reason)
	}
	onStop()
	close(h.done)
}

func (h *Hub) load(ctx context.Context) error {
	sctx, cancel := h.storeContext(ctx)
	defer cancel()
	stored, err := h.store.Load(sctx, h.id)
	if err != nil {
		return fmt.Errorf("load: %w", err)
	}

	doc, err := crdt.Restore(0, stored.Snapshot) // the hub only applies, never creates operations
	if err != nil {
		return fmt.Errorf("load: restore snapshot %d: %w", stored.SnapshotSeq, err)
	}
	for i, op := range stored.Ops {
		if err := doc.Apply(op); err != nil {
			return fmt.Errorf("load: apply op %d: %w", stored.SnapshotSeq+uint64(i)+1, err)
		}
	}
	h.doc = doc
	h.snapshotSeq = stored.SnapshotSeq

	cctx, ccancel := h.storeContext(ctx)
	comments, err := h.store.Comments(cctx, h.id)
	ccancel()
	if err != nil {
		return fmt.Errorf("load comments: %w", err)
	}
	h.comments = make(map[int64]Comment, len(comments))
	for _, c := range comments {
		h.comments[c.ID] = c
	}
	h.seq = stored.SnapshotSeq + uint64(len(stored.Ops))
	h.logger.Info("document loaded", "seq", h.seq, "snapshot_seq", h.snapshotSeq, "ops_replayed", len(stored.Ops))
	return nil
}

func (h *Hub) loop(ctx context.Context) error {
	idle := time.NewTimer(h.cfg.IdleTimeout)
	defer idle.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-idle.C:
			h.logger.Info("unloading idle document")
			return nil
		case req := <-h.join:
			c, err := h.addClient(ctx, req.JoinRequest)
			req.reply <- joinResult{client: c, err: err}
			if c != nil && len(req.Pending) > 0 {
				if err := h.accept(ctx, c, req.Pending, time.Now()); err != nil {
					return err
				}
			}
		case c := <-h.leave:
			h.drop(c, nil)
		case s := <-h.submit:
			if err := h.handle(ctx, s); err != nil {
				return err
			}
		case users := <-h.access:
			for c := range h.clients {
				if slices.Contains(users, c.userID) {
					h.drop(c, ErrAccessChanged)
				}
			}
		case req := <-h.versions:
			v, err := h.saveVersion(ctx, req.userID, req.name)
			req.reply <- versionResult{version: v, err: err}
		case req := <-h.restores:
			seq, fatal, err := h.restore(ctx, req.userID, req.versionID)
			if fatal {
				// run wraps err in ErrStorage for everyone else.
				req.reply <- restoreResult{err: fmt.Errorf("%w: %w", ErrStorage, err)}
				return err
			}
			req.reply <- restoreResult{seq: seq, err: err}
		}

		h.announceDepartures()
		if len(h.clients) == 0 {
			idle.Reset(h.cfg.IdleTimeout)
		} else {
			idle.Stop()
		}
	}
}

func (h *Hub) addClient(ctx context.Context, req JoinRequest) (*Client, error) {
	replica, err := h.replicaFor(ctx, req.UserID, req.Replica)
	if err != nil {
		return nil, err
	}

	// The same replica reconnecting means its old connection is dead even
	// if the server has not noticed yet: take over.
	for other := range h.clients {
		if other.replica == replica {
			h.drop(other, ErrReplaced)
		}
	}

	c := &Client{
		replica: replica, userID: req.UserID, userName: req.UserName, role: req.Role,
		out: make(chan any, max(h.cfg.QueueSize, 1)),
	}
	w := h.welcome(ctx, replica, req.LastSeq)
	w.Role = req.Role
	c.out <- w
	h.clients[c] = struct{}{}

	// What the snapshot does not carry: comments and the others' cursors.
	// The client resets both on every Welcome, so empty lists are not sent.
	if len(h.comments) > 0 {
		h.send(c, Comments{List: h.activeComments()})
	}
	for other := range h.clients {
		if other != c && other.cursor != nil {
			h.send(c, presenceOf(other))
		}
	}
	h.logger.Debug("client joined", "replica", replica, "user", req.UserID, "role", req.Role, "resumed", req.Replica != 0,
		"last_seq", req.LastSeq, "pending", len(req.Pending), "clients", len(h.clients))
	return c, nil
}

// replicaFor reserves a new replica for the user, or checks that the one
// to resume was issued to this very user: replica IDs are part of every
// character ID, so taking over somebody else's would let a client forge
// their edits.
func (h *Hub) replicaFor(ctx context.Context, userID int64, resume uint32) (uint32, error) {
	sctx, cancel := h.storeContext(ctx)
	defer cancel()
	if resume != 0 {
		owner, err := h.store.ReplicaOwner(sctx, h.id, resume)
		switch {
		case errors.Is(err, ErrNoReplica) || err == nil && owner != userID:
			// The same answer for "never issued" and "issued to someone
			// else": a client learns nothing about other users' replicas.
			return 0, fmt.Errorf("%w: %d", ErrUnknownReplica, resume)
		case err != nil:
			return 0, fmt.Errorf("%w: replica owner: %w", ErrStorage, err)
		}
		return resume, nil
	}
	replica, err := h.store.NextReplica(sctx, h.id, userID)
	if err != nil {
		h.logger.Error("cannot reserve a replica", "err", err)
		return 0, fmt.Errorf("%w: reserve replica: %w", ErrStorage, err)
	}
	return replica, nil
}

// welcome sends a client that saw lastSeq only the operations it missed if
// there are not too many of them, and the full snapshot otherwise.
func (h *Hub) welcome(ctx context.Context, replica uint32, lastSeq uint64) Welcome {
	w := Welcome{Replica: replica, Seq: h.seq}
	if lastSeq > 0 && lastSeq <= h.seq && h.seq-lastSeq <= h.cfg.MaxCatchUp {
		sctx, cancel := h.storeContext(ctx)
		ops, err := h.store.LoadOps(sctx, h.id, lastSeq)
		cancel()
		if err == nil && uint64(len(ops)) == h.seq-lastSeq {
			w.CatchUp = true
			w.Ops = ops
			return w
		}
		h.logger.Warn("cannot load ops for catch-up, sending a snapshot", "last_seq", lastSeq, "loaded", len(ops), "err", err)
	}
	// lastSeq > h.seq means the client saw operations the store has lost
	// (e.g. a restored backup): only a snapshot brings it back in line.
	w.Snapshot = h.doc.Snapshot()
	return w
}

// accept applies the operations one by one. Operations that have already
// taken effect (resent after a lost Ack) are skipped, the new ones are
// stored and broadcast. On an invalid operation the operations before it
// are still kept and the sender is dropped, since a correct client never
// sends one. A store failure is returned and stops the hub.
func (h *Hub) accept(ctx context.Context, c *Client, ops []crdt.Op, received time.Time) error {
	if _, ok := h.clients[c]; !ok {
		return nil // dropped while the batch was in flight
	}
	if !c.role.CanEdit() {
		// A client of a viewer or commenter never offers to edit; one that
		// does is not trusted with anything else on this connection.
		h.drop(c, fmt.Errorf("%w: %s", ErrForbidden, c.role))
		return nil
	}

	var invalid error
	var fresh []crdt.Op
	for _, op := range ops {
		if invalid = h.check(c, op); invalid != nil {
			break
		}
		applied := h.doc.Applied(op)
		if invalid = h.doc.Apply(op); invalid != nil {
			break // also catches an insert that reuses an ID with other content
		}
		if !applied {
			fresh = append(fresh, op)
		}
	}

	if err := h.commit(ctx, fresh, c, received); err != nil {
		return err
	}
	if invalid != nil {
		h.drop(c, fmt.Errorf("%w: %w", ErrInvalidOp, invalid))
		return nil
	}
	h.send(c, Ack{Seq: h.seq})
	return nil
}

// commit stores operations that are already applied to h.doc, numbers
// them and sends them to every client except the author (nil: everyone).
// A store failure is returned and must stop the hub: the in-memory state is
// then ahead of the store.
func (h *Hub) commit(ctx context.Context, ops []crdt.Op, author *Client, received time.Time) error {
	if len(ops) == 0 {
		return nil
	}
	sctx, cancel := h.storeContext(ctx)
	err := h.store.AppendOps(sctx, h.id, h.seq+1, ops)
	cancel()
	if err != nil {
		return fmt.Errorf("append ops %d-%d: %w", h.seq+1, h.seq+uint64(len(ops)), err)
	}
	h.seq += uint64(len(ops))
	opsTotal.Add(float64(len(ops)))

	for c := range h.clients {
		if c != author {
			h.send(c, Update{Seq: h.seq, Ops: ops, Received: received})
		}
	}
	if h.seq-h.snapshotSeq >= h.cfg.SnapshotEvery {
		h.saveSnapshot(ctx)
	}
	return nil
}

func (h *Hub) saveVersion(ctx context.Context, userID int64, name string) (Version, error) {
	sctx, cancel := h.storeContext(ctx)
	defer cancel()
	v, err := h.store.SaveVersion(sctx, h.id, Version{Name: name, Seq: h.seq, CreatedBy: userID}, h.doc.Snapshot())
	if err != nil {
		return Version{}, fmt.Errorf("%w: save version: %w", ErrStorage, err)
	}
	h.logger.Info("version saved", "version", v.ID, "seq", v.Seq, "user", userID)
	return v, nil
}

// restore reverts the text to a version. Failures before the document is
// touched are only reported; fatal is set when the in-memory state may be
// ahead of the store, and the hub must stop.
func (h *Hub) restore(ctx context.Context, userID, versionID int64) (seq uint64, fatal bool, err error) {
	received := time.Now()
	sctx, cancel := h.storeContext(ctx)
	_, state, err := h.store.Version(sctx, h.id, versionID)
	cancel()
	switch {
	case errors.Is(err, ErrNoVersion):
		return 0, false, err
	case err != nil:
		return 0, false, fmt.Errorf("%w: load version: %w", ErrStorage, err)
	}

	// New characters get a fresh replica of the user: the log shows who
	// restored the version.
	replica, err := h.replicaFor(ctx, userID, 0)
	if err != nil {
		return 0, false, err
	}
	ops, err := h.doc.RevertTo(state, replica)
	if err != nil {
		return 0, false, fmt.Errorf("version %d does not fit the document: %w", versionID, err)
	}
	for _, op := range ops {
		if err := h.doc.Apply(op); err != nil {
			return 0, true, fmt.Errorf("apply revert operation: %w", err)
		}
	}
	if err := h.commit(ctx, ops, nil, received); err != nil {
		return 0, true, err
	}
	h.logger.Info("version restored", "version", versionID, "ops", len(ops), "seq", h.seq, "user", userID, "replica", replica)
	return h.seq, false, nil
}

// check enforces what the CRDT itself cannot know: a client may create only
// IDs of its own replica, and every dependency must already be applied
// (a client only refers to characters it received from the hub or created
// itself and sent earlier on the same connection).
func (h *Hub) check(c *Client, op crdt.Op) error {
	switch op := op.(type) {
	case crdt.Insert:
		if op.ID.Replica != c.replica {
			return fmt.Errorf("insert %v from replica %d: foreign replica", op.ID, c.replica)
		}
		if !h.doc.Contains(op.After) {
			return fmt.Errorf("insert %v: unknown after %v", op.ID, op.After)
		}
	case crdt.Delete:
		if !h.doc.Contains(op.ID) {
			return fmt.Errorf("delete of unknown %v", op.ID)
		}
	}
	return nil
}

// saveSnapshot stores the current state if it is newer than the last
// snapshot. A failure only costs a longer replay on the next load, so it is
// logged and not treated as fatal.
func (h *Hub) saveSnapshot(ctx context.Context) {
	if h.seq == h.snapshotSeq {
		return
	}
	sctx, cancel := h.storeContext(ctx)
	defer cancel()
	if err := h.store.SaveSnapshot(sctx, h.id, h.seq, h.doc.Snapshot()); err != nil {
		h.logger.Warn("cannot save snapshot", "seq", h.seq, "err", err)
		return
	}
	h.snapshotSeq = h.seq
	h.logger.Debug("snapshot saved", "seq", h.seq)
}

// storeContext bounds a storage call. It is detached from ctx so that the
// final snapshot and operations accepted during shutdown are still stored.
func (h *Hub) storeContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), h.cfg.StoreTimeout)
}

func (h *Hub) send(c *Client, msg any) {
	select {
	case c.out <- msg:
	default:
		h.drop(c, ErrSlowClient)
	}
}

func (h *Hub) drop(c *Client, err error) {
	if _, ok := h.clients[c]; !ok {
		return
	}
	delete(h.clients, c)
	if c.cursor != nil {
		// Announced by the loop once the current event is handled: sending
		// from here could drop another client in the middle of a broadcast.
		h.departed = append(h.departed, c.replica)
	}
	c.err = err
	close(c.out)
	h.logger.Debug("client left", "replica", c.replica, "reason", err, "clients", len(h.clients))
}

// handle dispatches one client message.
func (h *Hub) handle(ctx context.Context, s submission) error {
	if _, ok := h.clients[s.client]; !ok {
		return nil // dropped while the message was in flight
	}
	switch {
	case s.cursor != nil:
		h.setCursor(s.client, *s.cursor)
	case s.comment != nil:
		h.addComment(ctx, s.client, *s.comment)
	case s.resolve != 0:
		h.resolveComment(ctx, s.client, s.resolve)
	default:
		return h.accept(ctx, s.client, s.ops, s.received)
	}
	return nil
}

func (h *Hub) setCursor(c *Client, cursor Cursor) {
	// The hub knows every character a correct client can point at; a
	// cursor at an unknown one is ignored rather than treated as an attack.
	if !h.doc.Contains(cursor.Anchor) || !h.doc.Contains(cursor.Head) {
		return
	}
	c.cursor = &cursor
	msg := presenceOf(c)
	for other := range h.clients {
		if other != c {
			h.send(other, msg)
		}
	}
}

func presenceOf(c *Client) Presence {
	return Presence{Replica: c.replica, UserID: c.userID, UserName: c.userName, Cursor: c.cursor}
}

// announceDepartures removes the cursors of clients that left.
func (h *Hub) announceDepartures() {
	for len(h.departed) > 0 {
		replica := h.departed[0]
		h.departed = h.departed[1:]
		for c := range h.clients {
			h.send(c, Presence{Replica: replica})
		}
	}
}

func (h *Hub) addComment(ctx context.Context, c *Client, nc newComment) {
	if !c.role.CanComment() {
		h.drop(c, fmt.Errorf("%w: %s cannot comment", ErrForbidden, c.role))
		return
	}
	body := strings.TrimSpace(nc.body)
	if n := utf8.RuneCountInString(body); n < 1 || n > maxCommentBody {
		h.drop(c, fmt.Errorf("%w: comment must be 1-%d characters", ErrInvalidOp, maxCommentBody))
		return
	}
	// Both ends must be characters the hub knows, in document order. The
	// zero ID is not a character and cannot be commented.
	if nc.from == (crdt.ID{}) || nc.to == (crdt.ID{}) || !h.doc.InOrder(nc.from, nc.to) {
		h.drop(c, fmt.Errorf("%w: comment range %v..%v", ErrInvalidOp, nc.from, nc.to))
		return
	}

	sctx, cancel := h.storeContext(ctx)
	comment, err := h.store.SaveComment(sctx, h.id, Comment{From: nc.from, To: nc.to, Body: body, AuthorID: c.userID})
	cancel()
	if err != nil {
		// Nothing in memory changed: the hub goes on, only this client is
		// told (by closing) that its comment did not make it.
		h.logger.Error("cannot save comment", "err", err)
		h.drop(c, fmt.Errorf("%w: save comment: %w", ErrStorage, err))
		return
	}
	comment.AuthorName = c.userName
	h.comments[comment.ID] = comment
	h.logger.Debug("comment added", "comment", comment.ID, "user", c.userID)
	for other := range h.clients {
		h.send(other, CommentChanged{Comment: comment})
	}
}

func (h *Hub) resolveComment(ctx context.Context, c *Client, id int64) {
	comment, ok := h.comments[id]
	if !ok {
		return // already resolved, e.g. by someone else at the same time
	}
	if comment.AuthorID != c.userID && !c.role.CanEdit() {
		h.drop(c, fmt.Errorf("%w: only the author or an editor resolves a comment", ErrForbidden))
		return
	}
	sctx, cancel := h.storeContext(ctx)
	err := h.store.ResolveComment(sctx, h.id, id, c.userID)
	cancel()
	if err != nil {
		h.logger.Error("cannot resolve comment", "err", err)
		h.drop(c, fmt.Errorf("%w: resolve comment: %w", ErrStorage, err))
		return
	}
	delete(h.comments, id)
	comment.Resolved = true
	for other := range h.clients {
		h.send(other, CommentChanged{Comment: comment})
	}
}

func (h *Hub) activeComments() []Comment {
	list := make([]Comment, 0, len(h.comments))
	for _, c := range h.comments {
		list = append(list, c)
	}
	slices.SortFunc(list, func(a, b Comment) int { return cmp.Compare(a.ID, b.ID) })
	return list
}
