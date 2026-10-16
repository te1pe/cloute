package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"cloute/internal/auth"
	"cloute/internal/config"
	"cloute/internal/crdt"
	"cloute/internal/crdt/wire"
	"cloute/internal/storage/postgres/pgtest"
)

// server is one run of the real server on a random port.
type server struct {
	addr string
	stop func()
}

func startServer(t *testing.T, dbURL string) server {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	cfg := config.Config{
		HTTPAddr:        ln.Addr().String(),
		DatabaseURL:     dbURL,
		LogFormat:       config.LogFormatText,
		LogLevel:        slog.LevelInfo,
		ShutdownTimeout: 5 * time.Second,
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		done <- run(ctx, cfg, logger, ln)
	}()

	stopped := false
	stop := func() {
		if stopped {
			return
		}
		stopped = true
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("run() = %v, want nil", err)
			}
		case <-time.After(10 * time.Second):
			t.Error("run() did not return after context cancellation")
		}
	}
	t.Cleanup(stop)
	return server{addr: ln.Addr().String(), stop: stop}
}

func TestRunServesAndShutsDown(t *testing.T) {
	srv := startServer(t, pgtest.URL(t))
	client := &http.Client{
		Timeout:   5 * time.Second,
		Transport: &http.Transport{DisableKeepAlives: true},
	}
	url := "http://" + srv.addr + "/healthz"

	resp, err := client.Get(url)
	if err != nil {
		t.Fatalf("GET /healthz: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusOK)
	}

	srv.stop()
	if resp, err := client.Get(url); err == nil {
		_ = resp.Body.Close()
		t.Fatal("server still accepts connections after shutdown")
	}
}

// wsClient speaks just enough of the protocol for this test.
// account registers a new user through the API and returns the session
// token from the cookie, as a browser would keep it.
func account(t *testing.T, addr, name string) string {
	t.Helper()
	body := fmt.Sprintf(`{"email":%q,"name":%q,"password":"correct horse"}`,
		"u-"+strings.ToLower(rand.Text())+"@example.test", name)
	resp, err := http.Post("http://"+addr+"/api/register", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	for _, c := range resp.Cookies() {
		if c.Name == auth.SessionCookie {
			return c.Value
		}
	}
	t.Fatalf("register: status %d, no session cookie", resp.StatusCode)
	return ""
}

// call sends a JSON API request with the session cookie.
func call(t *testing.T, addr, token, method, path string, body, out any) int {
	t.Helper()
	var r io.Reader
	if body != nil {
		data, _ := json.Marshal(body)
		r = bytes.NewReader(data)
	}
	req, _ := http.NewRequest(method, "http://"+addr+path, r)
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(&http.Cookie{Name: auth.SessionCookie, Value: token})
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			t.Fatalf("%s %s: %v", method, path, err)
		}
	}
	return resp.StatusCode
}

// newDocument creates a document owned by the token's user.
func newDocument(t *testing.T, addr, token string) string {
	t.Helper()
	var d struct{ ID string }
	if code := call(t, addr, token, "POST", "/api/documents", map[string]string{"title": "Отчёт"}, &d); code != http.StatusCreated {
		t.Fatalf("create document: %d", code)
	}
	return d.ID
}

// share gives the user behind token the role through a link of the owner.
func share(t *testing.T, addr, ownerToken, docID, role, token string) {
	t.Helper()
	var l struct{ Token string }
	if code := call(t, addr, ownerToken, "POST", "/api/documents/"+docID+"/links", map[string]string{"role": role}, &l); code != http.StatusCreated {
		t.Fatalf("create %s link: %d", role, code)
	}
	var d struct{ Role string }
	if code := call(t, addr, token, "POST", "/api/links/"+l.Token+"/join", nil, &d); code != http.StatusOK || d.Role != role {
		t.Fatalf("join by %s link: %d, role %q", role, code, d.Role)
	}
}

func dialWS(t *testing.T, addr, token string) *websocket.Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	header := http.Header{}
	header.Set("Cookie", (&http.Cookie{Name: auth.SessionCookie, Value: token}).String())
	conn, _, err := websocket.Dial(ctx, "ws://"+addr+"/ws", &websocket.DialOptions{HTTPHeader: header})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.CloseNow() })
	return conn
}

type wsClient struct {
	t       *testing.T
	conn    *websocket.Conn
	token   string
	role    string
	replica uint32
	doc     *crdt.Doc
	seq     uint64
}

func connect(t *testing.T, addr, docID, token string) *wsClient {
	t.Helper()
	c := &wsClient{t: t, conn: dialWS(t, addr, token), token: token}
	c.send(map[string]any{"v": 1, "type": "hello", "docId": docID})

	var welcome struct {
		Type     string        `json:"type"`
		Role     string        `json:"role"`
		Replica  uint32        `json:"replica"`
		Seq      uint64        `json:"seq"`
		Snapshot wire.Snapshot `json:"snapshot"`
	}
	c.receive(&welcome)
	if welcome.Type != "welcome" {
		t.Fatalf("want welcome, got %q", welcome.Type)
	}
	c.role = welcome.Role
	entries, err := wire.DecodeSnapshot(welcome.Snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if c.doc, err = crdt.Restore(welcome.Replica, entries); err != nil {
		t.Fatal(err)
	}
	c.replica = welcome.Replica
	c.seq = welcome.Seq
	return c
}

func (c *wsClient) send(v any) {
	c.t.Helper()
	data, _ := json.Marshal(v)
	if err := c.conn.Write(c.t.Context(), websocket.MessageText, data); err != nil {
		c.t.Fatalf("write: %v", err)
	}
}

func (c *wsClient) receive(v any) {
	c.t.Helper()
	ctx, cancel := context.WithTimeout(c.t.Context(), 5*time.Second)
	defer cancel()
	_, data, err := c.conn.Read(ctx)
	if err != nil {
		c.t.Fatalf("read: %v", err)
	}
	if err := json.Unmarshal(data, v); err != nil {
		c.t.Fatalf("decode %s: %v", data, err)
	}
}

// answerClose keeps reading in the background, as a browser does, so that
// the client answers the server's close frame on shutdown.
func (c *wsClient) answerClose() {
	c.conn.CloseRead(context.Background())
}

// typeText inserts text at pos and waits for the server to store it.
func (c *wsClient) typeText(pos int, text string) {
	c.t.Helper()
	var ops []crdt.Op
	for i, r := range []rune(text) {
		op, err := c.doc.InsertAt(pos+i, r)
		if err != nil {
			c.t.Fatal(err)
		}
		ops = append(ops, op)
	}
	c.send(map[string]any{"v": 1, "type": "ops", "ops": wire.EncodeOps(ops)})

	var ack struct {
		Type string `json:"type"`
		Seq  uint64 `json:"seq"`
	}
	c.receive(&ack)
	if ack.Type != "ack" || ack.Seq != c.seq+uint64(len(ops)) {
		c.t.Fatalf("got %+v, want ack for seq %d", ack, c.seq+uint64(len(ops)))
	}
	c.seq = ack.Seq
}

// TestEditsSurviveRestart is the stage 3 acceptance test: edits made before
// a server restart are there after it, and new clients get fresh replicas.
func TestEditsSurviveRestart(t *testing.T) {
	dbURL := pgtest.URL(t)
	srv := startServer(t, dbURL)
	token := account(t, srv.addr, "Alice")
	docID := newDocument(t, srv.addr, token)
	alice := connect(t, srv.addr, docID, token)
	alice.typeText(0, "Отчёт 👋")
	alice.answerClose()
	srv.stop()

	srv = startServer(t, dbURL)
	bob := connect(t, srv.addr, docID, token)
	if bob.doc.String() != "Отчёт 👋" || bob.seq != 7 {
		t.Fatalf("after restart: text %q, seq %d; want %q, 7", bob.doc.String(), bob.seq, "Отчёт 👋")
	}
	// Replicas are reserved in the database, so they are not reused.
	if bob.replica <= alice.replica {
		t.Fatalf("replica %d after restart is not above %d", bob.replica, alice.replica)
	}

	bob.typeText(bob.doc.Len(), " готов")
	bob.answerClose()
	srv.stop()

	srv = startServer(t, dbURL)
	carol := connect(t, srv.addr, docID, token)
	if want := "Отчёт 👋 готов"; carol.doc.String() != want {
		t.Fatalf("after second restart: %q, want %q", carol.doc.String(), want)
	}
}

// resume reconnects a client that kept its replica and has pending ops, as
// the web client does after being offline.
func resume(t *testing.T, addr, docID string, c *wsClient, pending []crdt.Op) {
	t.Helper()
	c.conn = dialWS(t, addr, c.token)
	c.send(map[string]any{
		"v": 1, "type": "hello", "docId": docID,
		"replica": c.replica, "lastSeq": c.seq, "pending": wire.EncodeOps(pending),
	})

	var welcome struct {
		Type    string    `json:"type"`
		Replica uint32    `json:"replica"`
		Seq     uint64    `json:"seq"`
		Ops     []wire.Op `json:"ops"`
	}
	c.receive(&welcome)
	if welcome.Type != "welcome" || welcome.Replica != c.replica || welcome.Ops == nil {
		t.Fatalf("welcome = %+v, want catch-up for replica %d", welcome, c.replica)
	}
	ops, err := wire.DecodeOps(welcome.Ops)
	if err != nil {
		t.Fatal(err)
	}
	for _, op := range ops {
		if err := c.doc.Apply(op); err != nil {
			t.Fatal(err)
		}
	}
	var ack struct {
		Type string `json:"type"`
		Seq  uint64 `json:"seq"`
	}
	c.receive(&ack)
	if ack.Type != "ack" || ack.Seq != welcome.Seq+uint64(len(pending)) {
		t.Fatalf("got %+v, want ack for seq %d", ack, welcome.Seq+uint64(len(pending)))
	}
	c.seq = ack.Seq
}

// TestOfflineEditsAcrossRestart is the stage 4 acceptance test end to end:
// Bob goes offline, the server restarts meanwhile, Alice and Bob both edit,
// Bob comes back with his replica and pending ops, and no edit is lost.
func TestOfflineEditsAcrossRestart(t *testing.T) {
	dbURL := pgtest.URL(t)
	srv := startServer(t, dbURL)
	aliceToken, bobToken := account(t, srv.addr, "Alice"), account(t, srv.addr, "Bob")
	docID := newDocument(t, srv.addr, aliceToken)
	share(t, srv.addr, aliceToken, docID, "editor", bobToken)

	alice := connect(t, srv.addr, docID, aliceToken)
	alice.typeText(0, "Отчёт")
	bob := connect(t, srv.addr, docID, bobToken)
	_ = bob.conn.CloseNow() // Bob's Wi-Fi is gone
	alice.answerClose()
	srv.stop()

	srv = startServer(t, dbURL)
	alice = connect(t, srv.addr, docID, aliceToken)
	alice.typeText(alice.doc.Len(), " готов")

	var pending []crdt.Op // Bob types offline at the start
	for i, r := range []rune("Наш ") {
		op, err := bob.doc.InsertAt(i, r)
		if err != nil {
			t.Fatal(err)
		}
		pending = append(pending, op)
	}
	resume(t, srv.addr, docID, bob, pending)

	var upd struct {
		Type string    `json:"type"`
		Seq  uint64    `json:"seq"`
		Ops  []wire.Op `json:"ops"`
	}
	alice.receive(&upd)
	ops, err := wire.DecodeOps(upd.Ops)
	if err != nil {
		t.Fatal(err)
	}
	for _, op := range ops {
		if err := alice.doc.Apply(op); err != nil {
			t.Fatal(err)
		}
	}

	const want = "Наш Отчёт готов"
	if alice.doc.String() != want || bob.doc.String() != want {
		t.Fatalf("alice %q, bob %q; want both %q", alice.doc.String(), bob.doc.String(), want)
	}
}

// TestRolesEndToEnd is the stage 5 acceptance test through the real server:
// links give roles, viewers and commenters read but their operations are
// refused, editors write.
func TestRolesEndToEnd(t *testing.T) {
	srv := startServer(t, pgtest.URL(t))
	owner := account(t, srv.addr, "Студент")
	docID := newDocument(t, srv.addr, owner)
	writer := connect(t, srv.addr, docID, owner)
	writer.typeText(0, "Отчёт")

	for _, role := range []string{"viewer", "commenter"} {
		t.Run(role, func(t *testing.T) {
			token := account(t, srv.addr, role)
			share(t, srv.addr, owner, docID, role, token)
			c := connect(t, srv.addr, docID, token)
			if c.role != role || c.doc.String() != "Отчёт" {
				t.Fatalf("%s: role %q, text %q", role, c.role, c.doc.String())
			}

			op, _ := c.doc.InsertAt(0, '!')
			c.send(map[string]any{"v": 1, "type": "ops", "ops": wire.EncodeOps([]crdt.Op{op})})
			var e struct{ Type, Code string }
			c.receive(&e)
			if e.Type != "error" || e.Code != "forbidden" {
				t.Fatalf("%s sent ops: got %+v, want error forbidden", role, e)
			}
		})
	}

	editor := account(t, srv.addr, "editor")
	share(t, srv.addr, owner, docID, "editor", editor)
	c := connect(t, srv.addr, docID, editor)
	c.typeText(c.doc.Len(), " готов")

	stranger := account(t, srv.addr, "stranger")
	conn := dialWS(t, srv.addr, stranger)
	data, _ := json.Marshal(map[string]any{"v": 1, "type": "hello", "docId": docID})
	_ = conn.Write(t.Context(), websocket.MessageText, data)
	_, msg, err := conn.Read(t.Context())
	if err != nil || !strings.Contains(string(msg), `"forbidden"`) {
		t.Fatalf("stranger: %s, %v; want forbidden", msg, err)
	}

	final := connect(t, srv.addr, docID, owner)
	if final.doc.String() != "Отчёт готов" {
		t.Fatalf("document is %q: refused operations must not get in", final.doc.String())
	}
}

// syncUntil reads broadcasts and acks until the client has seen seq.
func (c *wsClient) syncUntil(seq uint64) {
	c.t.Helper()
	for c.seq < seq {
		var m struct {
			Type string    `json:"type"`
			Seq  uint64    `json:"seq"`
			Ops  []wire.Op `json:"ops"`
		}
		c.receive(&m)
		if m.Type == "ops" {
			ops, err := wire.DecodeOps(m.Ops)
			if err != nil {
				c.t.Fatal(err)
			}
			for _, op := range ops {
				if err := c.doc.Apply(op); err != nil {
					c.t.Fatal(err)
				}
			}
		} else if m.Type != "ack" {
			c.t.Fatalf("unexpected %q", m.Type)
		}
		c.seq = max(c.seq, m.Seq)
	}
}

// TestRestoreDuringConcurrentEdit is the stage 6 acceptance test through the
// real server: the owner restores a version while an editor, who has not
// seen the restore yet, types. The editor's edit is not lost.
func TestRestoreDuringConcurrentEdit(t *testing.T) {
	srv := startServer(t, pgtest.URL(t))
	ownerToken, bobToken := account(t, srv.addr, "Студент"), account(t, srv.addr, "Боб")
	docID := newDocument(t, srv.addr, ownerToken)
	share(t, srv.addr, ownerToken, docID, "editor", bobToken)

	owner := connect(t, srv.addr, docID, ownerToken)
	owner.typeText(0, "Версия 1")
	var v struct {
		ID  int64
		Seq uint64
	}
	if code := call(t, srv.addr, ownerToken, "POST", "/api/documents/"+docID+"/versions", map[string]string{"name": "сдано"}, &v); code != http.StatusCreated || v.Seq != 8 {
		t.Fatalf("save version: %d %+v", code, v)
	}
	owner.typeText(owner.doc.Len(), ", черновик")

	bob := connect(t, srv.addr, docID, bobToken)
	excl, _ := bob.doc.InsertAt(bob.doc.Len(), '!') // typed before the restore reaches Bob

	var restored struct{ Seq uint64 }
	if code := call(t, srv.addr, ownerToken, "POST", "/api/documents/"+docID+"/versions/"+fmt.Sprint(v.ID)+"/restore", nil, &restored); code != http.StatusOK {
		t.Fatalf("restore: %d", code)
	}
	bob.send(map[string]any{"v": 1, "type": "ops", "ops": wire.EncodeOps([]crdt.Op{excl})})

	final := restored.Seq + 1
	bob.syncUntil(final)
	owner.syncUntil(final)

	const want = "Версия 1!"
	if owner.doc.String() != want || bob.doc.String() != want {
		t.Fatalf("owner %q, bob %q; want both %q", owner.doc.String(), bob.doc.String(), want)
	}

	// History is not rewritten: the version is still there, and a newcomer
	// sees the merged text.
	var text struct{ Text string }
	call(t, srv.addr, ownerToken, "GET", "/api/documents/"+docID+"/versions/"+fmt.Sprint(v.ID), nil, &text)
	if text.Text != "Версия 1" {
		t.Fatalf("version text %q", text.Text)
	}
	if c := connect(t, srv.addr, docID, ownerToken); c.doc.String() != want || c.seq != final {
		t.Fatalf("newcomer: %q at seq %d", c.doc.String(), c.seq)
	}
}

// rangeText is the visible text of the characters from..to in document
// order: what a comment anchored to them covers.
func rangeText(d *crdt.Doc, from, to crdt.ID) string {
	var b strings.Builder
	inside := false
	for _, e := range d.Snapshot() {
		if e.ID == from {
			inside = true
		}
		if inside && !e.Deleted {
			b.WriteRune(e.Char)
		}
		if e.ID == to {
			break
		}
	}
	return b.String()
}

// TestCommentStaysOnItsText is the stage 7 acceptance test for comments
// through the real server: a commenter's comment survives edits before and
// inside its range and a server restart, and still covers the same text.
func TestCommentStaysOnItsText(t *testing.T) {
	dbURL := pgtest.URL(t)
	srv := startServer(t, dbURL)
	studentToken, teacherToken := account(t, srv.addr, "Студент"), account(t, srv.addr, "Преподаватель")
	docID := newDocument(t, srv.addr, studentToken)
	share(t, srv.addr, studentToken, docID, "commenter", teacherToken)

	student := connect(t, srv.addr, docID, studentToken)
	student.typeText(0, "Введение. Выводы.")
	teacher := connect(t, srv.addr, docID, teacherToken)

	snap := teacher.doc.Snapshot()
	from, to := snap[10].ID, snap[15].ID // "Выводы"
	if got := rangeText(teacher.doc, from, to); got != "Выводы" {
		t.Fatalf("setup: range is %q", got)
	}
	teacher.send(map[string]any{"v": 1, "type": "comment", "from": wire.EncodeID(from), "to": wire.EncodeID(to), "text": "раскрыть подробнее"})
	var created struct {
		Type    string
		Comment struct{ ID int64 }
	}
	teacher.receive(&created)
	if created.Type != "comment" || created.Comment.ID == 0 {
		t.Fatalf("got %+v", created)
	}

	// Edits before the range and inside it.
	student.receive(&struct{}{}) // the comment broadcast
	student.typeText(0, "Аннотация. ")
	student.typeText(student.doc.Len()-4, "и ") // "Выв|оды." -> "Выви оды."
	student.answerClose()
	srv.stop()

	srv = startServer(t, dbURL)
	reader := connect(t, srv.addr, docID, studentToken)
	var list struct {
		Type     string
		Comments []struct {
			Text     string
			From, To wire.ID
			Author   struct{ Name string }
		}
	}
	reader.receive(&list)
	if list.Type != "comments" || len(list.Comments) != 1 {
		t.Fatalf("after restart: %+v", list)
	}
	c := list.Comments[0]
	cf, _ := wire.DecodeID(c.From)
	ct, _ := wire.DecodeID(c.To)
	if c.Text != "раскрыть подробнее" || c.Author.Name != "Преподаватель" {
		t.Fatalf("comment = %+v", c)
	}
	if got := rangeText(reader.doc, cf, ct); got != "Выви оды" {
		t.Fatalf("comment now covers %q in %q; want %q", got, reader.doc.String(), "Выви оды")
	}
}

// metrics reads /metrics into name -> value (labelled series keep their
// labels in the name).
func metrics(t *testing.T, addr string) map[string]float64 {
	t.Helper()
	resp, err := http.Get("http://" + addr + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	out := make(map[string]float64)
	for _, line := range strings.Split(string(body), "\n") {
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		i := strings.LastIndexByte(line, ' ')
		var v float64
		if _, err := fmt.Sscan(line[i+1:], &v); err == nil {
			out[line[:i]] = v
		}
	}
	return out
}

// TestMetrics checks the four metrics of stage 8 by their changes: the
// counters are process-wide, other tests move them too.
func TestMetrics(t *testing.T) {
	srv := startServer(t, pgtest.URL(t))
	before := metrics(t, srv.addr)
	for _, name := range []string{"cloute_ws_connections", "cloute_documents_loaded", "cloute_ops_total", "cloute_broadcast_seconds_count", "go_goroutines"} {
		if _, ok := before[name]; !ok {
			t.Fatalf("/metrics has no %s", name)
		}
	}

	token := account(t, srv.addr, "Метрики")
	docID := newDocument(t, srv.addr, token)
	writer := connect(t, srv.addr, docID, token)
	reader := connect(t, srv.addr, docID, token)
	writer.typeText(0, "привет")
	reader.syncUntil(6)

	after := metrics(t, srv.addr)
	delta := func(name string) float64 { return after[name] - before[name] }
	if delta("cloute_ws_connections") != 2 {
		t.Errorf("connections grew by %v, want 2", delta("cloute_ws_connections"))
	}
	if delta("cloute_documents_loaded") != 1 {
		t.Errorf("documents loaded grew by %v, want 1", delta("cloute_documents_loaded"))
	}
	if delta("cloute_ops_total") != 6 {
		t.Errorf("ops grew by %v, want 6", delta("cloute_ops_total"))
	}
	// One broadcast (the batch) to one other client.
	if delta("cloute_broadcast_seconds_count") != 1 {
		t.Errorf("broadcasts observed: %v, want 1", delta("cloute_broadcast_seconds_count"))
	}
}

func TestFrontendFallback(t *testing.T) {
	srv := startServer(t, pgtest.URL(t))
	resp, err := http.Get("http://" + srv.addr + "/")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	// Tests are built without -tags embedweb: the server says how to get a UI.
	if resp.StatusCode != http.StatusNotFound || !strings.Contains(string(body), "make build") {
		t.Fatalf("GET /: %d %q", resp.StatusCode, body)
	}
}

// TestRevokedLinkEndsLiveSession reproduces both reported bugs through the
// real server: revoking a link must cut the open session of whoever joined
// through it, and a user who now has only a viewer link must not comment.
func TestRevokedLinkEndsLiveSession(t *testing.T) {
	srv := startServer(t, pgtest.URL(t))
	ownerToken, teacherToken := account(t, srv.addr, "Студент"), account(t, srv.addr, "Преподаватель")
	docID := newDocument(t, srv.addr, ownerToken)
	owner := connect(t, srv.addr, docID, ownerToken)
	owner.typeText(0, "текст")

	var commenterLink struct {
		ID    int64
		Token string
	}
	call(t, srv.addr, ownerToken, "POST", "/api/documents/"+docID+"/links", map[string]string{"role": "commenter"}, &commenterLink)
	call(t, srv.addr, teacherToken, "POST", "/api/links/"+commenterLink.Token+"/join", nil, nil)
	teacher := connect(t, srv.addr, docID, teacherToken)
	if teacher.role != "commenter" {
		t.Fatalf("teacher's role %q", teacher.role)
	}

	if code := call(t, srv.addr, ownerToken, "DELETE", "/api/documents/"+docID+"/links/"+fmt.Sprint(commenterLink.ID), nil, nil); code != http.StatusNoContent {
		t.Fatalf("revoke: %d", code)
	}
	// The open session is closed at once...
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	_, _, err := teacher.conn.Read(ctx)
	if websocket.CloseStatus(err) != websocket.StatusNormalClosure {
		t.Fatalf("teacher's connection: %v, want closed for an access change", err)
	}
	// ...and coming back is refused.
	conn := dialWS(t, srv.addr, teacherToken)
	data, _ := json.Marshal(map[string]any{"v": 1, "type": "hello", "docId": docID})
	_ = conn.Write(t.Context(), websocket.MessageText, data)
	_, msg, err := conn.Read(t.Context())
	if err != nil || !strings.Contains(string(msg), `"forbidden"`) {
		t.Fatalf("reconnect after revoke: %s, %v; want forbidden", msg, err)
	}

	// A viewer link now makes the teacher a viewer, who cannot comment.
	share(t, srv.addr, ownerToken, docID, "viewer", teacherToken)
	reader := connect(t, srv.addr, docID, teacherToken)
	if reader.role != "viewer" {
		t.Fatalf("role after the viewer link: %q", reader.role)
	}
	first := reader.doc.Snapshot()[0].ID
	reader.send(map[string]any{"v": 1, "type": "comment", "from": wire.EncodeID(first), "to": wire.EncodeID(first), "text": "x"})
	var e struct{ Type, Code string }
	reader.receive(&e)
	if e.Type != "error" || e.Code != "forbidden" {
		t.Fatalf("viewer's comment: %+v, want forbidden", e)
	}
}
