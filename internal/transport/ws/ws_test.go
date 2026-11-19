package ws

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"

	"cloute/internal/auth"
	"cloute/internal/crdt"
	"cloute/internal/crdt/wire"
	"cloute/internal/doc"
	"cloute/internal/doc/doctest"
)

var testConfig = doc.Config{
	QueueSize:     256,
	IdleTimeout:   time.Minute,
	SnapshotEvery: 1000,
	MaxCatchUp:    1000,
	StoreTimeout:  5 * time.Second,
}

func newRegistry() (*doc.Registry, *slog.Logger) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	return doc.NewRegistry(doctest.NewMemStore(), testConfig, logger), logger
}

type fakeAccess struct {
	roles map[string]auth.Role
}

func (a fakeAccess) UserBySession(_ context.Context, token string) (auth.User, error) {
	id, ok := strings.CutPrefix(token, "user-")
	n, err := strconv.ParseInt(id, 10, 64)
	if !ok || err != nil {
		return auth.User{}, auth.ErrUnauthenticated
	}
	return auth.User{ID: n, Name: token}, nil
}

func (a fakeAccess) Document(_ context.Context, docID string, userID int64) (auth.Document, error) {
	role, ok := a.roles[fmt.Sprintf("%s/%d", docID, userID)]
	if !ok {
		role = auth.RoleEditor
	}
	if role == "" {
		return auth.Document{}, auth.ErrNotFound
	}
	return auth.Document{ID: docID, Role: role}, nil
}

var lastUser atomic.Int64

func newUser() string {
	return fmt.Sprintf("user-%d", lastUser.Add(1))
}

func startServer(t *testing.T) string {
	t.Helper()
	return startServerWith(t, fakeAccess{})
}

func startServerWith(t *testing.T, access Access) string {
	t.Helper()
	registry, logger := newRegistry()
	srv := httptest.NewServer(NewHandler(registry, access, logger))
	t.Cleanup(func() {
		registry.Close()
		srv.Close()
	})
	return "ws" + strings.TrimPrefix(srv.URL, "http")
}

func dial(t *testing.T, url string) *websocket.Conn {
	t.Helper()
	return dialAs(t, url, newUser())
}

func dialAs(t *testing.T, url, token string) *websocket.Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	opts := &websocket.DialOptions{HTTPHeader: http.Header{}}
	if token != "" {
		opts.HTTPHeader.Set("Cookie", (&http.Cookie{Name: auth.SessionCookie, Value: token}).String())
	}
	conn, _, err := websocket.Dial(ctx, url, opts)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.CloseNow() })
	return conn
}

func send(t *testing.T, conn *websocket.Conn, v any) {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	if err := conn.Write(t.Context(), websocket.MessageText, data); err != nil {
		t.Fatalf("write: %v", err)
	}
}

type serverMessage struct {
	V        int           `json:"v"`
	Type     string        `json:"type"`
	Replica  uint32        `json:"replica"`
	Role     string        `json:"role"`
	Seq      uint64        `json:"seq"`
	Snapshot wire.Snapshot `json:"snapshot"`
	Ops      []wire.Op     `json:"ops"`
	Code     string        `json:"code"`
	Message  string        `json:"message"`
}

func receive(t *testing.T, conn *websocket.Conn) (serverMessage, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	_, data, err := conn.Read(ctx)
	if err != nil {
		return serverMessage{}, err
	}
	var m serverMessage
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("bad server message %s: %v", data, err)
	}
	if m.V != protocolVersion {
		t.Fatalf("server message without version: %s", data)
	}
	return m, nil
}

type testClient struct {
	t       *testing.T
	conn    *websocket.Conn
	token   string
	replica uint32
	doc     *crdt.Doc
	lastSeq uint64
}

func connect(t *testing.T, url, docID string) *testClient {
	t.Helper()
	return connectAs(t, url, docID, newUser())
}

func connectAs(t *testing.T, url, docID, token string) *testClient {
	t.Helper()
	conn := dialAs(t, url, token)
	send(t, conn, helloMessage{envelope: header(typeHello), DocID: docID})
	m, err := receive(t, conn)
	if err != nil || m.Type != typeWelcome {
		t.Fatalf("want welcome, got %+v, %v", m, err)
	}
	entries, err := wire.DecodeSnapshot(m.Snapshot)
	if err != nil {
		t.Fatal(err)
	}
	d, err := crdt.Restore(m.Replica, entries)
	if err != nil {
		t.Fatal(err)
	}
	return &testClient{t: t, conn: conn, token: token, replica: m.Replica, doc: d, lastSeq: m.Seq}
}

func (c *testClient) apply(w []wire.Op) {
	c.t.Helper()
	ops, err := wire.DecodeOps(w)
	if err != nil {
		c.t.Fatal(err)
	}
	for _, op := range ops {
		if err := c.doc.Apply(op); err != nil {
			c.t.Fatal(err)
		}
	}
}

func (c *testClient) typeText(pos int, text string) {
	c.t.Helper()
	for i, r := range []rune(text) {
		op, err := c.doc.InsertAt(pos+i, r)
		if err != nil {
			c.t.Fatal(err)
		}
		send(c.t, c.conn, opsMessage{envelope: header(typeOps), Ops: wire.EncodeOps([]crdt.Op{op})})
	}
}

func (c *testClient) syncUntil(seq uint64) error {
	for c.lastSeq < seq {
		m, err := receive(c.t, c.conn)
		if err != nil {
			return err
		}
		switch m.Type {
		case typeAck:
		case typeOps:
			c.apply(m.Ops)
		default:
			return errors.New("unexpected message " + m.Type + ": " + m.Message)
		}
		c.lastSeq = max(c.lastSeq, m.Seq)
	}
	return nil
}

func TestTwoClientsConverge(t *testing.T) {
	url := startServer(t)
	alice := connect(t, url, "report")
	bob := connect(t, url, "report")
	if alice.doc == nil || bob.doc == nil {
		t.Fatal("no welcome")
	}

	alice.typeText(0, "hello")
	if err := bob.syncUntil(5); err != nil {
		t.Fatal(err)
	}
	if got := bob.doc.String(); got != "hello" {
		t.Fatalf("bob sees %q, want %q", got, "hello")
	}

	const aliceText, bobText = " мир 👋", ">> "
	var wg sync.WaitGroup
	wg.Go(func() { alice.typeText(alice.doc.Len(), aliceText) })
	wg.Go(func() { bob.typeText(0, bobText) })
	wg.Wait()

	total := uint64(5 + len([]rune(aliceText)) + len([]rune(bobText)))
	for _, c := range []*testClient{alice, bob} {
		if err := c.syncUntil(total); err != nil {
			t.Fatal(err)
		}
	}

	want := ">> hello мир 👋"
	if alice.doc.String() != want || bob.doc.String() != want {
		t.Fatalf("diverged or wrong:\nalice %q\nbob   %q\nwant  %q", alice.doc.String(), bob.doc.String(), want)
	}

	pos := alice.doc.Len()
	wg.Go(func() { alice.typeText(pos, "AAAA") })
	wg.Go(func() { bob.typeText(pos, "bbbb") })
	wg.Wait()
	total += 8
	for _, c := range []*testClient{alice, bob} {
		if err := c.syncUntil(total); err != nil {
			t.Fatal(err)
		}
	}
	if alice.doc.String() != bob.doc.String() {
		t.Fatalf("diverged:\nalice %q\nbob   %q", alice.doc.String(), bob.doc.String())
	}
	if got := alice.doc.String(); !strings.Contains(got, "AAAA") || !strings.Contains(got, "bbbb") {
		t.Fatalf("concurrent runs interleaved: %q", got)
	}
	want = alice.doc.String()

	carol := connect(t, url, "report")
	if carol.lastSeq != total || carol.doc.String() != want {
		t.Fatalf("late joiner: seq %d, text %q; want seq %d, %q", carol.lastSeq, carol.doc.String(), total, want)
	}

	other := connect(t, url, "other")
	if other.lastSeq != 0 || other.doc.String() != "" {
		t.Fatalf("other document is not empty: seq %d, %q", other.lastSeq, other.doc.String())
	}
}

func TestProtocolErrors(t *testing.T) {
	foreign := opsMessage{envelope: header(typeOps), Ops: []wire.Op{{
		Type: "insert", ID: wire.ID{1, 999}, After: &wire.ID{0, 0}, Char: "x",
	}}}

	tests := []struct {
		name     string
		messages []any
		wantCode string
	}{
		{"wrong version", []any{map[string]any{"v": 2, "type": "hello", "docId": "d"}}, codeUnsupportedVersion},
		{"ops before hello", []any{opsMessage{envelope: header(typeOps)}}, codeBadRequest},
		{"bad doc id", []any{helloMessage{envelope: header(typeHello), DocID: "../etc"}}, codeBadRequest},
		{"unknown type", []any{map[string]any{"v": 1, "type": "launch"}}, codeBadRequest},
		{"empty ops", []any{
			helloMessage{envelope: header(typeHello), DocID: "d"},
			opsMessage{envelope: header(typeOps), Ops: []wire.Op{}},
		}, codeInvalidOp},
		{"foreign replica", []any{helloMessage{envelope: header(typeHello), DocID: "d"}, foreign}, codeInvalidOp},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			conn := dial(t, startServer(t))
			for _, m := range tt.messages {
				send(t, conn, m)
			}
			for {
				m, err := receive(t, conn)
				if err != nil {
					t.Fatalf("connection closed without an error message: %v", err)
				}
				if m.Type == typeWelcome {
					continue
				}
				if m.Type != typeError || m.Code != tt.wantCode {
					t.Fatalf("got %+v, want error %q", m, tt.wantCode)
				}
				break
			}
			if _, err := receive(t, conn); websocket.CloseStatus(err) != websocket.StatusPolicyViolation {
				t.Fatalf("close status = %v (%v), want PolicyViolation", websocket.CloseStatus(err), err)
			}
		})
	}
}

func TestServerShutdownClosesConnections(t *testing.T) {
	registry, logger := newRegistry()
	srv := httptest.NewServer(NewHandler(registry, fakeAccess{}, logger))
	defer srv.Close()

	c := connect(t, "ws"+strings.TrimPrefix(srv.URL, "http"), "d")
	registry.Close()

	if _, err := receive(t, c.conn); websocket.CloseStatus(err) != websocket.StatusGoingAway {
		t.Fatalf("close status = %v (%v), want GoingAway", websocket.CloseStatus(err), err)
	}
}

func TestHandlerShutdown(t *testing.T) {
	registry, logger := newRegistry()
	handler := NewHandler(registry, fakeAccess{}, logger)
	srv := httptest.NewServer(handler)
	defer srv.Close()
	url := "ws" + strings.TrimPrefix(srv.URL, "http")

	polite := connect(t, url, "d")
	polite.conn.CloseRead(context.Background())
	silent := connect(t, url, "d")

	short, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	if err := handler.Shutdown(short); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Shutdown with open connections = %v, want DeadlineExceeded", err)
	}
	if _, _, err := silent.conn.Read(t.Context()); websocket.CloseStatus(err) != -1 {
		t.Fatalf("silent client: %v, want a dropped connection", err)
	}

	if _, resp, err := websocket.Dial(t.Context(), url, nil); err == nil || resp == nil || resp.StatusCode != 503 {
		t.Fatalf("dial after Shutdown: err = %v, resp = %v; want 503", err, resp)
	}
	registry.Close()
}

func TestHandlerShutdownWaitsForHandshake(t *testing.T) {
	registry, logger := newRegistry()
	handler := NewHandler(registry, fakeAccess{}, logger)
	srv := httptest.NewServer(handler)
	defer srv.Close()

	c := connect(t, "ws"+strings.TrimPrefix(srv.URL, "http"), "d")
	c.conn.CloseRead(context.Background())

	registry.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err := handler.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown after registry.Close = %v, want nil", err)
	}
}

func TestOfflineReconnect(t *testing.T) {
	url := startServer(t)
	alice := connect(t, url, "report")
	alice.typeText(0, "AB")
	bob := connect(t, url, "report")
	if bob.doc.String() != "AB" {
		t.Fatalf("bob sees %q", bob.doc.String())
	}

	_ = bob.conn.CloseNow()
	alice.typeText(2, "cd")
	if err := alice.syncUntil(4); err != nil {
		t.Fatal(err)
	}

	var pending []crdt.Op
	for i, r := range []rune("xy") {
		op, _ := bob.doc.InsertAt(1+i, r)
		pending = append(pending, op)
	}
	del, _ := bob.doc.DeleteAt(3)
	pending = append(pending, del)

	conn := dialAs(t, url, bob.token)
	send(t, conn, helloMessage{
		envelope: header(typeHello), DocID: "report",
		Replica: bob.replica, LastSeq: bob.lastSeq, Pending: wire.EncodeOps(pending),
	})
	w, err := receive(t, conn)
	if err != nil || w.Type != typeWelcome || w.Replica != bob.replica || w.Snapshot.Chars != nil || len(w.Ops) != 2 {
		t.Fatalf("welcome = %+v, %v; want catch-up with 2 ops for replica %d", w, err, bob.replica)
	}
	bob.conn = conn
	bob.apply(w.Ops)
	if err := bob.syncUntil(4 + uint64(len(pending))); err != nil {
		t.Fatal(err)
	}
	if err := alice.syncUntil(4 + uint64(len(pending))); err != nil {
		t.Fatal(err)
	}

	const want = "Axycd"
	if alice.doc.String() != want || bob.doc.String() != want {
		t.Fatalf("alice %q, bob %q; want both %q", alice.doc.String(), bob.doc.String(), want)
	}
}

func TestWelcomeForms(t *testing.T) {
	url := startServer(t)
	a := connect(t, url, "d")
	a.typeText(0, "hi")

	for _, tt := range []struct {
		lastSeq     uint64
		wantCatchUp bool
	}{{0, false}, {2, true}} {
		conn := dialAs(t, url, a.token)
		send(t, conn, helloMessage{envelope: header(typeHello), DocID: "d", Replica: a.replica, LastSeq: tt.lastSeq})
		_, data, err := conn.Read(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		var raw map[string]json.RawMessage
		if err := json.Unmarshal(data, &raw); err != nil {
			t.Fatal(err)
		}
		_, hasOps := raw["ops"]
		_, hasSnapshot := raw["snapshot"]
		if hasOps != tt.wantCatchUp || hasSnapshot == tt.wantCatchUp {
			t.Fatalf("lastSeq %d: welcome %s", tt.lastSeq, data)
		}
		if tt.wantCatchUp && string(raw["ops"]) != "[]" {
			t.Fatalf("empty catch-up encoded as %s, want []", raw["ops"])
		}
		_ = conn.CloseNow()
	}
}

func TestUnknownReplicaIsBadRequest(t *testing.T) {
	conn := dial(t, startServer(t))
	send(t, conn, helloMessage{envelope: header(typeHello), DocID: "d", Replica: 42})
	m, err := receive(t, conn)
	if err != nil || m.Type != typeError || m.Code != codeBadRequest {
		t.Fatalf("got %+v, %v; want bad_request", m, err)
	}
}

func TestJoinDuringShutdown(t *testing.T) {
	registry, logger := newRegistry()
	handler := NewHandler(registry, fakeAccess{}, logger)

	served := make(chan any, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() { served <- recover() }()
		handler.ServeHTTP(w, r)
	}))
	defer srv.Close()
	registry.Close()

	conn := dial(t, "ws"+strings.TrimPrefix(srv.URL, "http"))
	send(t, conn, helloMessage{envelope: header(typeHello), DocID: "d"})
	if _, err := receive(t, conn); websocket.CloseStatus(err) != websocket.StatusGoingAway {
		t.Fatalf("close status = %v (%v), want GoingAway", websocket.CloseStatus(err), err)
	}
	select {
	case p := <-served:
		if p != nil {
			t.Fatalf("handler panicked: %v", p)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("handler did not finish")
	}
}

func TestPingDropsDeadClient(t *testing.T) {
	registry, logger := newRegistry()
	handler := NewHandler(registry, fakeAccess{}, logger)
	handler.pingInterval = 20 * time.Millisecond
	handler.pingTimeout = 50 * time.Millisecond
	srv := httptest.NewServer(handler)
	defer srv.Close()
	defer registry.Close()

	c := connect(t, "ws"+strings.TrimPrefix(srv.URL, "http"), "d")
	time.Sleep(300 * time.Millisecond)
	ctx0, cancel0 := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel0()
	for {
		if _, _, err := c.conn.Read(ctx0); err != nil {
			if ctx0.Err() != nil {
				t.Fatal("dead client was not dropped")
			}
			break
		}
	}

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err := handler.Shutdown(ctx); err != nil {
		t.Fatalf("connection still active after the ping timeout: %v", err)
	}
}

func TestAccessControl(t *testing.T) {
	access := fakeAccess{roles: map[string]auth.Role{
		"doc/101": auth.RoleViewer,
		"doc/102": auth.RoleCommenter,
		"doc/103": "",
	}}
	url := startServerWith(t, access)
	editor := connectAs(t, url, "doc", "user-100")
	editor.typeText(0, "text")

	expectError := func(t *testing.T, conn *websocket.Conn, code string) {
		t.Helper()
		for {
			m, err := receive(t, conn)
			if err != nil {
				t.Fatalf("closed without error %q: %v", code, err)
			}
			if m.Type == typeWelcome {
				continue
			}
			if m.Type != typeError || m.Code != code {
				t.Fatalf("got %+v, want error %q", m, code)
			}
			break
		}
		if _, err := receive(t, conn); websocket.CloseStatus(err) != websocket.StatusPolicyViolation {
			t.Fatalf("close status = %v (%v), want PolicyViolation", websocket.CloseStatus(err), err)
		}
	}

	t.Run("no session", func(t *testing.T) {
		conn := dialAs(t, url, "")
		send(t, conn, helloMessage{envelope: header(typeHello), DocID: "doc"})
		expectError(t, conn, codeUnauthorized)
	})

	t.Run("no role in the document", func(t *testing.T) {
		conn := dialAs(t, url, "user-103")
		send(t, conn, helloMessage{envelope: header(typeHello), DocID: "doc"})
		expectError(t, conn, codeForbidden)
	})

	for _, tt := range []struct{ token, role string }{{"user-101", "viewer"}, {"user-102", "commenter"}} {
		t.Run(tt.role+" reads but cannot edit", func(t *testing.T) {
			c := connectAs(t, url, "doc", tt.token)
			if c.doc.String() != "text" {
				t.Fatalf("%s sees %q", tt.role, c.doc.String())
			}
			op, _ := c.doc.InsertAt(0, '!')
			send(t, c.conn, opsMessage{envelope: header(typeOps), Ops: wire.EncodeOps([]crdt.Op{op})})
			expectError(t, c.conn, codeForbidden)
		})
	}

	late := connectAs(t, url, "doc", "user-104")
	if late.doc.String() != "text" || late.lastSeq != 4 {
		t.Fatalf("document is %q at seq %d, want %q at 4", late.doc.String(), late.lastSeq, "text")
	}
}

func TestWelcomeCarriesRole(t *testing.T) {
	url := startServerWith(t, fakeAccess{roles: map[string]auth.Role{"d/201": auth.RoleCommenter}})
	conn := dialAs(t, url, "user-201")
	send(t, conn, helloMessage{envelope: header(typeHello), DocID: "d"})
	m, err := receive(t, conn)
	if err != nil || m.Type != typeWelcome || m.Role != "commenter" {
		t.Fatalf("welcome = %+v, %v; want role commenter", m, err)
	}
}

func TestResumeOtherUsersReplicaIsRefused(t *testing.T) {
	url := startServer(t)
	alice := connect(t, url, "d")

	conn := dial(t, url)
	send(t, conn, helloMessage{envelope: header(typeHello), DocID: "d", Replica: alice.replica, LastSeq: alice.lastSeq})
	m, err := receive(t, conn)
	if err != nil || m.Type != typeError || m.Code != codeBadRequest {
		t.Fatalf("got %+v, %v; want bad_request", m, err)
	}
}

func TestCursorsAndComments(t *testing.T) {
	url := startServerWith(t, fakeAccess{roles: map[string]auth.Role{"doc/302": auth.RoleViewer}})
	alice := connectAs(t, url, "doc", "user-301")
	alice.typeText(0, "abc")
	if err := alice.syncUntil(3); err != nil {
		t.Fatal(err)
	}
	viewer := connectAs(t, url, "doc", "user-302")
	if err := viewer.syncUntil(3); err != nil {
		t.Fatal(err)
	}

	a, c := alice.doc.Snapshot()[0].ID, alice.doc.Snapshot()[2].ID
	send(t, alice.conn, map[string]any{"v": 1, "type": "presence", "cursor": map[string]any{
		"anchor": wire.EncodeID(a), "head": wire.EncodeID(c),
	}})
	_, data, err := viewer.conn.Read(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	var p struct {
		Type    string
		Replica uint32
		User    struct{ ID int64 }
		Cursor  struct{ Anchor, Head wire.ID }
	}
	if err := json.Unmarshal(data, &p); err != nil || p.Type != "presence" || p.Replica != alice.replica || p.User.ID != 301 || p.Cursor.Head != wire.EncodeID(c) {
		t.Fatalf("presence %s", data)
	}

	send(t, alice.conn, map[string]any{"v": 1, "type": "comment", "from": wire.EncodeID(a), "to": wire.EncodeID(c), "text": "всё слово"})
	for _, cl := range []*testClient{alice, viewer} {
		_, data, err := cl.conn.Read(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		var m struct {
			Type    string
			Comment struct {
				ID       int64
				From, To wire.ID
				Text     string
				Author   struct{ ID int64 }
				Resolved bool
			}
		}
		if err := json.Unmarshal(data, &m); err != nil || m.Type != "comment" || m.Comment.Text != "всё слово" || m.Comment.Author.ID != 301 || m.Comment.From != wire.EncodeID(a) {
			t.Fatalf("comment %s", data)
		}
	}

	send(t, viewer.conn, map[string]any{"v": 1, "type": "comment", "from": wire.EncodeID(a), "to": wire.EncodeID(a), "text": "x"})
	m, err := receive(t, viewer.conn)
	if err != nil || m.Type != typeError || m.Code != codeForbidden {
		t.Fatalf("viewer comment: %+v, %v; want forbidden", m, err)
	}

	conn := dialAs(t, url, "user-303")
	send(t, conn, helloMessage{envelope: header(typeHello), DocID: "doc"})
	var types []string
	for range 3 {
		_, data, err := conn.Read(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		var e envelope
		_ = json.Unmarshal(data, &e)
		types = append(types, e.Type)
	}
	if strings.Join(types, ",") != "welcome,comments,presence" {
		t.Fatalf("newcomer got %v", types)
	}
}
