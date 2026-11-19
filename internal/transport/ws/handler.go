package ws

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/coder/websocket"

	"cloute/internal/auth"
	"cloute/internal/crdt"
	"cloute/internal/crdt/wire"
	"cloute/internal/doc"
)

const (
	maxMessageSize = 1 << 20
	helloTimeout   = 10 * time.Second
	writeTimeout   = 10 * time.Second
	maxDocIDLength = 64
	pingInterval   = 30 * time.Second
	pingTimeout    = 10 * time.Second
)

type Access interface {
	UserBySession(ctx context.Context, token string) (auth.User, error)
	Document(ctx context.Context, docID string, userID int64) (auth.Document, error)
}

type Handler struct {
	registry *doc.Registry
	access   Access
	logger   *slog.Logger

	pingInterval time.Duration
	pingTimeout  time.Duration

	// Shutdown не ждёт hijacked-соединения
	mu      sync.Mutex
	closing bool
	active  sync.WaitGroup
	conns   map[*websocket.Conn]struct{}
}

func NewHandler(registry *doc.Registry, access Access, logger *slog.Logger) *Handler {
	return &Handler{
		registry:     registry,
		access:       access,
		logger:       logger,
		pingInterval: pingInterval,
		pingTimeout:  pingTimeout,
		conns:        make(map[*websocket.Conn]struct{}),
	}
}

func (h *Handler) Shutdown(ctx context.Context) error {
	h.mu.Lock()
	h.closing = true
	h.mu.Unlock()

	done := make(chan struct{})
	go func() {
		h.active.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
	}

	h.mu.Lock()
	for conn := range h.conns {
		_ = conn.CloseNow()
	}
	h.mu.Unlock()
	<-done
	return ctx.Err()
}

func (h *Handler) track() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closing {
		return false
	}
	h.active.Add(1)
	return true
}

func (h *Handler) register(conn *websocket.Conn) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.conns[conn] = struct{}{}
	connections.Inc()
}

func (h *Handler) unregister(conn *websocket.Conn) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.conns, conn)
	connections.Dec()
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !h.track() {
		http.Error(w, "server is shutting down", http.StatusServiceUnavailable)
		return
	}
	defer h.active.Done()

	// проверка Origin
	conn, err := websocket.Accept(w, r, nil)
	if err != nil {
		h.logger.Debug("websocket accept failed", "err", err)
		return
	}
	h.register(conn)
	defer func() {
		h.unregister(conn)
		_ = conn.CloseNow()
	}()
	conn.SetReadLimit(maxMessageSize)

	var token string
	if c, err := r.Cookie(auth.SessionCookie); err == nil {
		token = c.Value
	}

	ctx := r.Context()
	if err := h.serve(ctx, conn, token); err != nil {
		var perr *protocolError
		if errors.As(err, &perr) {
			h.logger.Info("closing connection on protocol error", "code", perr.code, "err", perr.message)
			_ = writeJSON(ctx, conn, errorMessage{envelope: header(typeError), Code: perr.code, Message: perr.message})
			_ = conn.Close(websocket.StatusPolicyViolation, perr.code)
			return
		}
		h.logger.Debug("connection closed", "err", err)
	}
}

func (h *Handler) serve(ctx context.Context, conn *websocket.Conn, token string) error {
	user, err := h.access.UserBySession(ctx, token)
	switch {
	case errors.Is(err, auth.ErrUnauthenticated):
		return &protocolError{code: codeUnauthorized, message: "log in first"}
	case err != nil:
		h.logger.Error("cannot check session", "err", err)
		_ = conn.Close(websocket.StatusInternalError, "storage unavailable")
		return err
	}

	hello, pending, err := readHello(ctx, conn)
	if err != nil {
		return err
	}

	document, err := h.access.Document(ctx, hello.DocID, user.ID)
	switch {
	case errors.Is(err, auth.ErrNotFound):
		return &protocolError{code: codeForbidden, message: "no access to this document"}
	case err != nil:
		h.logger.Error("cannot check access", "doc", hello.DocID, "err", err)
		_ = conn.Close(websocket.StatusInternalError, "storage unavailable")
		return err
	}

	req := doc.JoinRequest{
		UserID: user.ID, UserName: user.Name, Role: document.Role,
		Replica: hello.Replica, LastSeq: hello.LastSeq, Pending: pending,
	}
	hub, client, err := h.registry.Join(ctx, hello.DocID, req)
	switch {
	case errors.Is(err, doc.ErrHubClosed):
		_ = conn.Close(websocket.StatusGoingAway, "server shutting down")
		return err
	case errors.Is(err, doc.ErrUnknownReplica):
		return badRequest("%v", err)
	case err != nil:
		h.logger.Error("cannot open document", "doc", hello.DocID, "err", err)
		_ = conn.Close(websocket.StatusInternalError, "storage unavailable")
		return err
	}
	logger := h.logger.With("doc", hello.DocID, "replica", client.Replica(), "user", user.ID, "role", document.Role)
	logger.Info("client connected", "resumed", hello.Replica != 0, "last_seq", hello.LastSeq, "pending", len(pending))
	defer logger.Info("client disconnected")

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go h.pingLoop(ctx, conn, logger)

	writerDone := make(chan struct{})
	go func() {
		defer close(writerDone)
		writeLoop(ctx, conn, client, logger)
	}()

	err = readLoop(ctx, conn, hub, client)
	hub.Leave(client)
	<-writerDone
	return err
}

func (h *Handler) pingLoop(ctx context.Context, conn *websocket.Conn, logger *slog.Logger) {
	ticker := time.NewTicker(h.pingInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			pctx, cancel := context.WithTimeout(ctx, h.pingTimeout)
			err := conn.Ping(pctx)
			cancel()
			if err != nil {
				if ctx.Err() == nil {
					logger.Info("dropping unresponsive client", "err", err)
					_ = conn.CloseNow()
				}
				return
			}
		}
	}
}

func readHello(ctx context.Context, conn *websocket.Conn) (*helloMessage, []crdt.Op, error) {
	helloCtx, cancel := context.WithTimeout(ctx, helloTimeout)
	defer cancel()

	msg, err := readMessage(helloCtx, conn)
	if err != nil {
		return nil, nil, err
	}
	hello, ok := msg.(*helloMessage)
	if !ok {
		return nil, nil, badRequest("first message must be hello")
	}
	if !validDocID(hello.DocID) {
		return nil, nil, badRequest("docId must be 1-%d characters of [A-Za-z0-9_-]", maxDocIDLength)
	}
	if hello.LastSeq > wire.MaxSafeInteger {
		return nil, nil, badRequest("lastSeq %d exceeds 2^53-1", hello.LastSeq)
	}
	pending, err := wire.DecodeOps(hello.Pending)
	if err != nil {
		return nil, nil, &protocolError{code: codeInvalidOp, message: "pending " + err.Error()}
	}
	return hello, pending, nil
}

func readLoop(ctx context.Context, conn *websocket.Conn, hub *doc.Hub, client *doc.Client) error {
	for {
		msg, err := readMessage(ctx, conn)
		if err != nil {
			return err
		}
		switch m := msg.(type) {
		case *opsMessage:
			if len(m.Ops) == 0 {
				return &protocolError{code: codeInvalidOp, message: "empty ops list"}
			}
			ops, err := wire.DecodeOps(m.Ops)
			if err != nil {
				return &protocolError{code: codeInvalidOp, message: err.Error()}
			}
			err = hub.Submit(ctx, client, ops)
			if err != nil {
				return err
			}
		case *presenceMessage:
			if m.Cursor == nil {
				continue
			}
			anchor, err1 := wire.DecodeID(m.Cursor.Anchor)
			head, err2 := wire.DecodeID(m.Cursor.Head)
			if err := errors.Join(err1, err2); err != nil {
				return badRequest("cursor: %v", err)
			}
			if err := hub.SetCursor(ctx, client, doc.Cursor{Anchor: anchor, Head: head}); err != nil {
				return err
			}
		case *newCommentMessage:
			from, err1 := wire.DecodeID(m.From)
			to, err2 := wire.DecodeID(m.To)
			if err := errors.Join(err1, err2); err != nil {
				return badRequest("comment range: %v", err)
			}
			if err := hub.AddComment(ctx, client, from, to, m.Text); err != nil {
				return err
			}
		case *resolveMessage:
			if err := hub.ResolveComment(ctx, client, m.ID); err != nil {
				return err
			}
		default:
			return badRequest("unexpected hello after handshake")
		}
	}
}

func readMessage(ctx context.Context, conn *websocket.Conn) (any, error) {
	typ, data, err := conn.Read(ctx)
	if err != nil {
		return nil, err
	}
	if typ != websocket.MessageText {
		return nil, badRequest("binary messages are not supported")
	}
	return decodeClientMessage(data)
}

func writeLoop(ctx context.Context, conn *websocket.Conn, client *doc.Client, logger *slog.Logger) {
	for msg := range client.Messages() {
		var out any
		var received time.Time
		switch m := msg.(type) {
		case doc.Welcome:
			w := welcomeMessage{envelope: header(typeWelcome), Replica: m.Replica, Role: string(m.Role), Seq: m.Seq}
			if m.CatchUp {
				w.Ops = wire.EncodeOps(m.Ops)
			} else {
				snap := wire.EncodeSnapshot(m.Snapshot)
				w.Snapshot = &snap
			}
			out = w
		case doc.Ack:
			out = ackMessage{envelope: header(typeAck), Seq: m.Seq}
		case doc.Update:
			out = opsMessage{envelope: header(typeOps), Seq: m.Seq, Ops: wire.EncodeOps(m.Ops)}
			received = m.Received
		case doc.Presence:
			out = encodePresence(m)
		case doc.Comments:
			list := make([]commentJSON, 0, len(m.List))
			for _, c := range m.List {
				list = append(list, encodeComment(c))
			}
			out = commentsMessage{envelope: header(typeComments), Comments: list}
		case doc.CommentChanged:
			out = commentMessage{envelope: header(typeComment), Comment: encodeComment(m.Comment)}
		default:
			continue
		}
		if writeJSON(ctx, conn, out) == nil && !received.IsZero() {
			broadcastSeconds.Observe(time.Since(received).Seconds())
		}
	}

	err := client.Err()
	switch {
	case err == nil:
	case errors.Is(err, doc.ErrForbidden):
		logger.Warn("client acted beyond its role", "err", err)
		_ = writeJSON(ctx, conn, errorMessage{envelope: header(typeError), Code: codeForbidden, Message: "your role does not allow this"})
		_ = conn.Close(websocket.StatusPolicyViolation, codeForbidden)
	case errors.Is(err, doc.ErrInvalidOp):
		logger.Info("client sent an invalid operation", "err", err)
		_ = writeJSON(ctx, conn, errorMessage{envelope: header(typeError), Code: codeInvalidOp, Message: err.Error()})
		_ = conn.Close(websocket.StatusPolicyViolation, codeInvalidOp)
	case errors.Is(err, doc.ErrSlowClient):
		logger.Warn("dropping slow client")
		_ = conn.Close(websocket.StatusTryAgainLater, "client too slow")
	case errors.Is(err, doc.ErrHubClosed):
		_ = conn.Close(websocket.StatusGoingAway, "server shutting down")
	case errors.Is(err, doc.ErrAccessChanged):
		_ = conn.Close(websocket.StatusNormalClosure, "access changed")
	case errors.Is(err, doc.ErrReplaced):
		_ = conn.Close(websocket.StatusNormalClosure, "replaced by a newer connection")
	case errors.Is(err, doc.ErrStorage):
		_ = conn.Close(websocket.StatusInternalError, "storage unavailable")
	default:
		_ = conn.Close(websocket.StatusInternalError, "internal error")
	}
}

func writeJSON(ctx context.Context, conn *websocket.Conn, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, writeTimeout)
	defer cancel()
	return conn.Write(ctx, websocket.MessageText, data)
}

func validDocID(id string) bool {
	if id == "" || len(id) > maxDocIDLength {
		return false
	}
	for _, c := range []byte(id) {
		ok := c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_'
		if !ok {
			return false
		}
	}
	return true
}
