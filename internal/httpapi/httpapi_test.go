package httpapi_test

import (
	"bytes"
	"crypto/rand"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"

	"cloute/internal/auth"
	"cloute/internal/doc"
	"cloute/internal/httpapi"
	"cloute/internal/storage/postgres"
	"cloute/internal/storage/postgres/pgtest"
)

func newServer(t *testing.T) *httptest.Server {
	t.Helper()
	pool := pgtest.Pool(t)
	svc, err := auth.NewService(postgres.NewAuthStore(pool), auth.Config{
		BcryptCost: bcrypt.MinCost,
		SessionTTL: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	registry := doc.NewRegistry(postgres.NewStore(pool), doc.Config{
		QueueSize: 16, IdleTimeout: time.Minute, SnapshotEvery: 1000, MaxCatchUp: 1000, StoreTimeout: 5 * time.Second,
	}, logger)
	srv := httptest.NewServer(httpapi.NewHandler(svc, registry, httpapi.Options{SessionTTL: time.Hour}, logger))
	t.Cleanup(func() {
		srv.Close()
		registry.Close()
	})
	return srv
}

// client is one browser: it keeps its own cookies.
type client struct {
	t   *testing.T
	srv *httptest.Server
	hc  *http.Client
}

func newClient(t *testing.T, srv *httptest.Server) *client {
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	return &client{t: t, srv: srv, hc: &http.Client{Jar: jar, Timeout: 5 * time.Second}}
}

// do sends a JSON request and decodes a JSON answer into out (if not nil).
func (c *client) do(method, path string, body any, out any) int {
	c.t.Helper()
	var r io.Reader
	if body != nil {
		data, _ := json.Marshal(body)
		r = bytes.NewReader(data)
	}
	req, _ := http.NewRequest(method, c.srv.URL+path, r)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		c.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			c.t.Fatalf("%s %s: decode: %v", method, path, err)
		}
	}
	return resp.StatusCode
}

type user struct {
	ID    int64  `json:"id"`
	Email string `json:"email"`
	Name  string `json:"name"`
}

type document struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	Role  string `json:"role"`
}

type link struct {
	ID    int64  `json:"id"`
	Role  string `json:"role"`
	Token string `json:"token"`
}

type apiError struct {
	Error string `json:"error"`
}

func randomEmail() string {
	return "u-" + strings.ToLower(rand.Text()) + "@example.test"
}

// registered returns a logged-in client of a fresh user.
func registered(t *testing.T, srv *httptest.Server, name string) (*client, user) {
	t.Helper()
	c := newClient(t, srv)
	var u user
	if code := c.do("POST", "/api/register", map[string]string{"email": randomEmail(), "name": name, "password": "correct horse"}, &u); code != http.StatusCreated {
		t.Fatalf("register: status %d", code)
	}
	return c, u
}

func TestHealthz(t *testing.T) {
	c := newClient(t, newServer(t))
	var body map[string]string
	if code := c.do("GET", "/healthz", nil, &body); code != http.StatusOK || body["status"] != "ok" {
		t.Fatalf("healthz: %d %v", code, body)
	}
}

func TestAccounts(t *testing.T) {
	srv := newServer(t)
	c := newClient(t, srv)
	email := randomEmail()

	var u user
	if code := c.do("POST", "/api/register", map[string]string{"email": "  " + strings.ToUpper(email), "name": "Аня", "password": "secret-123"}, &u); code != http.StatusCreated {
		t.Fatalf("register: %d", code)
	}
	if u.Email != email || u.Name != "Аня" {
		t.Fatalf("registered %+v, want normalized e-mail %q", u, email)
	}

	var me user
	if code := c.do("GET", "/api/me", nil, &me); code != http.StatusOK || me.ID != u.ID {
		t.Fatalf("me after register: %d %+v", code, me)
	}

	var e apiError
	if code := newClient(t, srv).do("POST", "/api/register", map[string]string{"email": email, "name": "x", "password": "secret-123"}, &e); code != http.StatusConflict || e.Error != "email_taken" {
		t.Fatalf("duplicate e-mail: %d %+v", code, e)
	}

	if code := c.do("POST", "/api/logout", nil, nil); code != http.StatusNoContent {
		t.Fatalf("logout: %d", code)
	}
	if code := c.do("GET", "/api/me", nil, &e); code != http.StatusUnauthorized {
		t.Fatalf("me after logout: %d", code)
	}

	for _, pw := range []string{"wrong-password", ""} {
		if code := c.do("POST", "/api/login", map[string]string{"email": email, "password": pw}, &e); code != http.StatusUnauthorized || e.Error != "invalid_credentials" {
			t.Fatalf("login with %q: %d %+v", pw, code, e)
		}
	}
	if code := c.do("POST", "/api/login", map[string]string{"email": randomEmail(), "password": "secret-123"}, &e); code != http.StatusUnauthorized || e.Error != "invalid_credentials" {
		t.Fatalf("login of unknown e-mail: %d %+v (must look like a wrong password)", code, e)
	}
	if code := c.do("POST", "/api/login", map[string]string{"email": email, "password": "secret-123"}, &me); code != http.StatusOK || me.ID != u.ID {
		t.Fatalf("login: %d %+v", code, me)
	}
}

func TestRegisterValidation(t *testing.T) {
	c := newClient(t, newServer(t))
	for _, body := range []map[string]string{
		{"email": "no-at-sign", "name": "A", "password": "secret-123"},
		{"email": randomEmail(), "name": "   ", "password": "secret-123"},
		{"email": randomEmail(), "name": "A", "password": "short"},
		{"email": randomEmail(), "name": "A", "password": strings.Repeat("x", 73)},
	} {
		var e apiError
		if code := c.do("POST", "/api/register", body, &e); code != http.StatusBadRequest {
			t.Errorf("register %v: %d %+v, want 400", body, code, e)
		}
	}
}

func TestSessionCookieAttributes(t *testing.T) {
	srv := newServer(t)
	resp, err := http.Post(srv.URL+"/api/register", "application/json",
		strings.NewReader(`{"email":"`+randomEmail()+`","name":"A","password":"secret-123"}`))
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	var session *http.Cookie
	for _, c := range resp.Cookies() {
		if c.Name == auth.SessionCookie {
			session = c
		}
	}
	if session == nil || !session.HttpOnly || session.SameSite != http.SameSiteLaxMode || session.Path != "/" || len(session.Value) < 40 {
		t.Fatalf("session cookie = %+v", session)
	}
}

func TestDocumentsAndRoles(t *testing.T) {
	srv := newServer(t)
	owner, _ := registered(t, srv, "Owner")
	stranger, _ := registered(t, srv, "Stranger")

	var d document
	if code := owner.do("POST", "/api/documents", map[string]string{"title": "Отчёт"}, &d); code != http.StatusCreated || d.Role != "owner" || d.Title != "Отчёт" {
		t.Fatalf("create: %d %+v", code, d)
	}
	var list []document
	if code := owner.do("GET", "/api/documents", nil, &list); code != http.StatusOK || len(list) != 1 || list[0] != d {
		t.Fatalf("list: %d %+v", code, list)
	}

	// A user without a role cannot tell the document exists.
	var e apiError
	if code := stranger.do("GET", "/api/documents/"+d.ID, nil, &e); code != http.StatusNotFound {
		t.Fatalf("stranger reads the document: %d", code)
	}
	if code := stranger.do("GET", "/api/documents/"+d.ID+"/links", nil, &e); code != http.StatusNotFound {
		t.Fatalf("stranger lists links: %d", code)
	}
	if code := stranger.do("GET", "/api/documents", nil, &list); code != http.StatusOK || len(list) != 0 {
		t.Fatalf("stranger's list: %+v", list)
	}
}

// TestShareLinks is the stage 5 acceptance test for links: a link gives
// its role, never lowers one, only the owner manages links, and a revoked
// link stops working.
func TestShareLinks(t *testing.T) {
	srv := newServer(t)
	owner, _ := registered(t, srv, "Owner")
	var d document
	owner.do("POST", "/api/documents", map[string]string{"title": "Отчёт"}, &d)

	links := map[string]link{}
	for _, role := range []string{"viewer", "commenter", "editor"} {
		var l link
		if code := owner.do("POST", "/api/documents/"+d.ID+"/links", map[string]string{"role": role}, &l); code != http.StatusCreated || l.Role != role || len(l.Token) < 40 {
			t.Fatalf("create %s link: %d %+v", role, code, l)
		}
		links[role] = l
	}
	var e apiError
	if code := owner.do("POST", "/api/documents/"+d.ID+"/links", map[string]string{"role": "owner"}, &e); code != http.StatusBadRequest {
		t.Fatalf("owner link: %d, want 400", code)
	}

	teacher, _ := registered(t, srv, "Teacher")
	var joined document
	if code := teacher.do("POST", "/api/links/"+links["commenter"].Token+"/join", nil, &joined); code != http.StatusOK || joined.ID != d.ID || joined.Role != "commenter" {
		t.Fatalf("join as commenter: %d %+v", code, joined)
	}
	// A weaker link does not lower the role, a stronger one raises it.
	teacher.do("POST", "/api/links/"+links["viewer"].Token+"/join", nil, &joined)
	if joined.Role != "commenter" {
		t.Fatalf("viewer link lowered the role to %q", joined.Role)
	}
	teacher.do("POST", "/api/links/"+links["editor"].Token+"/join", nil, &joined)
	if joined.Role != "editor" {
		t.Fatalf("editor link gave %q", joined.Role)
	}
	owner.do("POST", "/api/links/"+links["viewer"].Token+"/join", nil, &joined)
	if joined.Role != "owner" {
		t.Fatalf("owner opening a viewer link became %q", joined.Role)
	}

	// Only the owner manages links.
	if code := teacher.do("POST", "/api/documents/"+d.ID+"/links", map[string]string{"role": "viewer"}, &e); code != http.StatusForbidden {
		t.Fatalf("editor creates a link: %d, want 403", code)
	}
	var list []link
	if code := owner.do("GET", "/api/documents/"+d.ID+"/links", nil, &list); code != http.StatusOK || len(list) != 3 || list[0].Token != "" {
		t.Fatalf("links: %d %+v (tokens must not be listed)", code, list)
	}

	// A revoked link no longer works.
	if code := owner.do("DELETE", "/api/documents/"+d.ID+"/links/"+itoa(links["viewer"].ID), nil, nil); code != http.StatusNoContent {
		t.Fatalf("revoke: %d", code)
	}
	late, _ := registered(t, srv, "Late")
	if code := late.do("POST", "/api/links/"+links["viewer"].Token+"/join", nil, &e); code != http.StatusNotFound {
		t.Fatalf("join by a revoked link: %d", code)
	}
	if code := late.do("POST", "/api/links/not-a-token/join", nil, &e); code != http.StatusNotFound {
		t.Fatalf("join by a made-up token: %d", code)
	}
	if code := owner.do("DELETE", "/api/documents/"+d.ID+"/links/"+itoa(links["viewer"].ID), nil, nil); code != http.StatusNotFound {
		t.Fatalf("second revoke: %d", code)
	}
}

func TestCSRFDefences(t *testing.T) {
	srv := newServer(t)
	c, _ := registered(t, srv, "A")

	// A request from another site's page.
	req, _ := http.NewRequest("POST", srv.URL+"/api/documents", strings.NewReader(`{"title":"x"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "https://evil.example")
	resp, err := c.hc.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-origin POST: %d, want 403", resp.StatusCode)
	}

	// What a plain HTML form can send.
	req, _ = http.NewRequest("POST", srv.URL+"/api/documents", strings.NewReader("title=x"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err = c.hc.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnsupportedMediaType {
		t.Fatalf("form-encoded POST: %d, want 415", resp.StatusCode)
	}
}

func itoa(n int64) string {
	b, _ := json.Marshal(n)
	return string(b)
}

type version struct {
	ID     int64   `json:"id"`
	Name   string  `json:"name"`
	Seq    uint64  `json:"seq"`
	Author string  `json:"author"`
	Text   *string `json:"text"`
}

func TestVersionsAccess(t *testing.T) {
	srv := newServer(t)
	owner, _ := registered(t, srv, "Студент")
	var d document
	owner.do("POST", "/api/documents", map[string]string{"title": "Отчёт"}, &d)
	base := "/api/documents/" + d.ID + "/versions"

	var v version
	if code := owner.do("POST", base, map[string]string{"name": "Черновик 1"}, &v); code != http.StatusCreated || v.Name != "Черновик 1" || v.Author != "Студент" || v.Seq != 0 {
		t.Fatalf("save: %d %+v", code, v)
	}
	var e apiError
	if code := owner.do("POST", base, map[string]string{"name": "  "}, &e); code != http.StatusBadRequest {
		t.Fatalf("empty name: %d", code)
	}

	// A viewer reads the history but cannot change anything.
	var l link
	owner.do("POST", "/api/documents/"+d.ID+"/links", map[string]string{"role": "viewer"}, &l)
	viewer, _ := registered(t, srv, "Читатель")
	viewer.do("POST", "/api/links/"+l.Token+"/join", nil, nil)

	var list []version
	if code := viewer.do("GET", base, nil, &list); code != http.StatusOK || len(list) != 1 || list[0].ID != v.ID || list[0].Text != nil {
		t.Fatalf("viewer lists versions: %d %+v", code, list)
	}
	var one version
	if code := viewer.do("GET", base+"/"+itoa(v.ID), nil, &one); code != http.StatusOK || one.Text == nil || *one.Text != "" {
		t.Fatalf("viewer reads a version: %d %+v", code, one)
	}
	if code := viewer.do("POST", base, map[string]string{"name": "моя"}, &e); code != http.StatusForbidden {
		t.Fatalf("viewer saves a version: %d, want 403", code)
	}
	if code := viewer.do("POST", base+"/"+itoa(v.ID)+"/restore", nil, &e); code != http.StatusForbidden {
		t.Fatalf("viewer restores a version: %d, want 403", code)
	}

	// The owner restores; unknown versions and strangers get 404.
	var restored map[string]uint64
	if code := owner.do("POST", base+"/"+itoa(v.ID)+"/restore", nil, &restored); code != http.StatusOK {
		t.Fatalf("restore: %d", code)
	}
	if code := owner.do("POST", base+"/999999999/restore", nil, &e); code != http.StatusNotFound {
		t.Fatalf("restore of a missing version: %d", code)
	}
	if code := owner.do("GET", base+"/not-a-number", nil, &e); code != http.StatusNotFound {
		t.Fatalf("bad version id: %d", code)
	}
	stranger, _ := registered(t, srv, "Чужой")
	if code := stranger.do("GET", base, nil, &e); code != http.StatusNotFound {
		t.Fatalf("stranger lists versions: %d", code)
	}

	// A version of one document is not reachable through another.
	var other document
	owner.do("POST", "/api/documents", map[string]string{"title": "Другой"}, &other)
	if code := owner.do("GET", "/api/documents/"+other.ID+"/versions/"+itoa(v.ID), nil, &e); code != http.StatusNotFound {
		t.Fatalf("version through another document: %d", code)
	}
}

// role returns the user's role in the document, or "" if the document is
// not theirs to see (404).
func role(t *testing.T, c *client, docID string) string {
	t.Helper()
	var d document
	switch code := c.do("GET", "/api/documents/"+docID, nil, &d); code {
	case http.StatusOK:
		return d.Role
	case http.StatusNotFound:
		return ""
	default:
		t.Fatalf("GET document: %d", code)
		return ""
	}
}

// TestRevokeTakesAccessBack: revoking a link takes back the role it gave;
// the role is the highest among the links still in force.
func TestRevokeTakesAccessBack(t *testing.T) {
	srv := newServer(t)
	owner, _ := registered(t, srv, "Owner")
	var d document
	owner.do("POST", "/api/documents", map[string]string{"title": "Отчёт"}, &d)
	links := map[string]link{}
	for _, r := range []string{"viewer", "commenter", "editor"} {
		var l link
		owner.do("POST", "/api/documents/"+d.ID+"/links", map[string]string{"role": r}, &l)
		links[r] = l
	}
	revoke := func(r string) {
		t.Helper()
		if code := owner.do("DELETE", "/api/documents/"+d.ID+"/links/"+itoa(links[r].ID), nil, nil); code != http.StatusNoContent {
			t.Fatalf("revoke %s: %d", r, code)
		}
	}

	user, _ := registered(t, srv, "Student")
	for _, r := range []string{"viewer", "commenter", "editor"} {
		user.do("POST", "/api/links/"+links[r].Token+"/join", nil, nil)
	}
	if got := role(t, user, d.ID); got != "editor" {
		t.Fatalf("after three links: %q, want editor", got)
	}

	revoke("editor")
	if got := role(t, user, d.ID); got != "commenter" {
		t.Fatalf("after revoking the editor link: %q, want commenter", got)
	}
	revoke("commenter")
	if got := role(t, user, d.ID); got != "viewer" {
		t.Fatalf("after revoking the commenter link: %q, want viewer", got)
	}
	revoke("viewer")
	if got := role(t, user, d.ID); got != "" {
		t.Fatalf("after revoking every link: %q, want no access", got)
	}
	var list []document
	if user.do("GET", "/api/documents", nil, &list); len(list) != 0 {
		t.Fatalf("the document is still listed: %+v", list)
	}

	// The owner does not depend on links.
	if got := role(t, owner, d.ID); got != "owner" {
		t.Fatalf("owner's role: %q", got)
	}
}

// TestViewerLinkAfterRevokedCommenterLink is the reported bug: a user who
// had a commenter link, then got a viewer link after the first one was
// revoked, kept commenting. Now the user is a viewer.
func TestViewerLinkAfterRevokedCommenterLink(t *testing.T) {
	srv := newServer(t)
	owner, _ := registered(t, srv, "Owner")
	var d document
	owner.do("POST", "/api/documents", map[string]string{"title": "Отчёт"}, &d)
	var commenter, viewer link
	owner.do("POST", "/api/documents/"+d.ID+"/links", map[string]string{"role": "commenter"}, &commenter)

	user, _ := registered(t, srv, "Reader")
	user.do("POST", "/api/links/"+commenter.Token+"/join", nil, nil)
	owner.do("DELETE", "/api/documents/"+d.ID+"/links/"+itoa(commenter.ID), nil, nil)
	owner.do("POST", "/api/documents/"+d.ID+"/links", map[string]string{"role": "viewer"}, &viewer)

	var joined document
	if code := user.do("POST", "/api/links/"+viewer.Token+"/join", nil, &joined); code != http.StatusOK || joined.Role != "viewer" {
		t.Fatalf("join by the viewer link: %d %q, want viewer", code, joined.Role)
	}
}
