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
	ErrHubClosed      = errors.New("doc: hub closed")
	ErrSlowClient     = errors.New("doc: client too slow")
	ErrInvalidOp      = errors.New("doc: invalid operation")
	ErrStorage        = errors.New("doc: storage failure")
	ErrUnknownReplica = errors.New("doc: unknown replica")
	ErrForbidden      = errors.New("doc: role does not allow editing")
	ErrAccessChanged  = errors.New("doc: access changed")
	ErrReplaced       = errors.New("doc: replaced by a newer connection")
)

type JoinRequest struct {
	UserID   int64
	UserName string
	Role     auth.Role
	Replica  uint32
	LastSeq  uint64
	Pending  []crdt.Op
}

type (
	Welcome struct {
		Replica  uint32
		Role     auth.Role
		Seq      uint64
		CatchUp  bool
		Snapshot []crdt.Entry
		Ops      []crdt.Op
	}
	Ack struct {
		Seq uint64
	}
	Update struct {
		Seq      uint64
		Ops      []crdt.Op
		Received time.Time
	}
	Presence struct {
		Replica  uint32
		UserID   int64
		UserName string
		Cursor   *Cursor
	}
	Comments struct {
		List []Comment
	}
	CommentChanged struct {
		Comment Comment
	}
)

// якоря по ID, не по offset
type Cursor struct {
	Anchor, Head crdt.ID
}

const maxCommentBody = 2000

type Config struct {
	QueueSize     int
	IdleTimeout   time.Duration
	SnapshotEvery uint64
	MaxCatchUp    uint64
	StoreTimeout  time.Duration
}

// не блокируемся на медленном клиенте
type Client struct {
	replica  uint32
	userID   int64
	userName string
	role     auth.Role
	cursor   *Cursor
	out      chan any
	err      error
}

func (c *Client) Replica() uint32 {
	return c.replica
}

func (c *Client) Messages() <-chan any {
	return c.out
}

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
	err      error

	doc         *crdt.Doc
	seq         uint64
	snapshotSeq uint64
	clients     map[*Client]struct{}
	comments    map[int64]Comment
	departed    []uint32
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

func (h *Hub) Leave(c *Client) {
	select {
	case h.leave <- c:
	case <-h.done:
	}
}

func (h *Hub) Submit(ctx context.Context, c *Client, ops []crdt.Op) error {
	return h.post(ctx, submission{client: c, ops: ops})
}

func (h *Hub) SetCursor(ctx context.Context, c *Client, cursor Cursor) error {
	return h.post(ctx, submission{client: c, cursor: &cursor})
}

func (h *Hub) AddComment(ctx context.Context, c *Client, from, to crdt.ID, body string) error {
	return h.post(ctx, submission{client: c, comment: &newComment{from: from, to: to, body: body}})
}

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

func (h *Hub) AccessChanged(ctx context.Context, userIDs []int64) error {
	select {
	case h.access <- userIDs:
		return nil
	case <-h.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

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

func (h *Hub) run(ctx context.Context, onStop func()) {
	err := h.load(ctx)
	if err == nil {
		err = h.loop(ctx)
	}

	reason := ErrHubClosed
	if err != nil {
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

	doc, err := crdt.Restore(0, stored.Snapshot)
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

func (h *Hub) replicaFor(ctx context.Context, userID int64, resume uint32) (uint32, error) {
	sctx, cancel := h.storeContext(ctx)
	defer cancel()
	if resume != 0 {
		owner, err := h.store.ReplicaOwner(sctx, h.id, resume)
		switch {
		case errors.Is(err, ErrNoReplica) || err == nil && owner != userID:
			// не палим, чья реплика
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
	w.Snapshot = h.doc.Snapshot()
	return w
}

func (h *Hub) accept(ctx context.Context, c *Client, ops []crdt.Op, received time.Time) error {
	if _, ok := h.clients[c]; !ok {
		return nil
	}
	if !c.role.CanEdit() {
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
			break
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

// отвязан от ctx клиента
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
		h.departed = append(h.departed, c.replica)
	}
	c.err = err
	close(c.out)
	h.logger.Debug("client left", "replica", c.replica, "reason", err, "clients", len(h.clients))
}

func (h *Hub) handle(ctx context.Context, s submission) error {
	if _, ok := h.clients[s.client]; !ok {
		return nil
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
	if nc.from == (crdt.ID{}) || nc.to == (crdt.ID{}) || !h.doc.InOrder(nc.from, nc.to) {
		h.drop(c, fmt.Errorf("%w: comment range %v..%v", ErrInvalidOp, nc.from, nc.to))
		return
	}

	sctx, cancel := h.storeContext(ctx)
	comment, err := h.store.SaveComment(sctx, h.id, Comment{From: nc.from, To: nc.to, Body: body, AuthorID: c.userID})
	cancel()
	if err != nil {
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
		return
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
