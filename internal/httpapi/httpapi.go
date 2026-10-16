// Package httpapi wires the REST endpoints of the server: accounts,
// documents, access links and version history. The API is described in api/http.md.
package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"cloute/internal/auth"
	"cloute/internal/doc"
)

const maxBodySize = 64 << 10 // bytes; requests are small JSON objects

// Options tunes the API.
type Options struct {
	// CookieSecure marks the session cookie Secure (HTTPS only). Off for
	// local development over plain HTTP.
	CookieSecure bool
	// SessionTTL is the cookie lifetime; it matches the session in the store.
	SessionTTL time.Duration
}

// Hubs is what the API asks of the running documents: version history and
// re-checking connections after an access change. *doc.Registry implements it.
type Hubs interface {
	AccessChanged(ctx context.Context, docID string, userIDs []int64) error
	SaveVersion(ctx context.Context, docID string, userID int64, name string) (doc.Version, error)
	RestoreVersion(ctx context.Context, docID string, userID, versionID int64) (uint64, error)
	Versions(ctx context.Context, docID string) ([]doc.Version, error)
	VersionText(ctx context.Context, docID string, versionID int64) (doc.Version, string, error)
}

type api struct {
	svc      *auth.Service
	versions Hubs
	opts     Options
	logger   *slog.Logger
}

// NewHandler returns the root HTTP handler with all routes registered.
func NewHandler(svc *auth.Service, versions Hubs, opts Options, logger *slog.Logger) http.Handler {
	a := &api{svc: svc, versions: versions, opts: opts, logger: logger}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", handleHealthz)

	mux.HandleFunc("POST /api/register", a.register)
	mux.HandleFunc("POST /api/login", a.login)
	mux.HandleFunc("POST /api/logout", a.logout)
	mux.HandleFunc("GET /api/me", a.withUser(a.me))

	mux.HandleFunc("GET /api/documents", a.withUser(a.documents))
	mux.HandleFunc("POST /api/documents", a.withUser(a.createDocument))
	mux.HandleFunc("GET /api/documents/{id}", a.withUser(a.document))
	mux.HandleFunc("GET /api/documents/{id}/links", a.withUser(a.links))
	mux.HandleFunc("POST /api/documents/{id}/links", a.withUser(a.createLink))
	mux.HandleFunc("DELETE /api/documents/{id}/links/{linkID}", a.withUser(a.revokeLink))
	mux.HandleFunc("POST /api/links/{token}/join", a.withUser(a.join))

	mux.HandleFunc("GET /api/documents/{id}/versions", a.withUser(a.listVersions))
	mux.HandleFunc("POST /api/documents/{id}/versions", a.withUser(a.saveVersion))
	mux.HandleFunc("GET /api/documents/{id}/versions/{versionID}", a.withUser(a.version))
	mux.HandleFunc("POST /api/documents/{id}/versions/{versionID}/restore", a.withUser(a.restoreVersion))

	return sameOrigin(mux)
}

// handleHealthz reports that the process is up and serving HTTP.
func handleHealthz(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	// A write error means the client is gone; there is nobody to report it to.
	_, _ = io.WriteString(w, `{"status":"ok"}`+"\n")
}

// sameOrigin refuses state-changing requests sent from another site
// (CSRF). SameSite=Lax already keeps the cookie off cross-site POSTs in
// current browsers; this is the second line of defence.
func sameOrigin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			if origin := r.Header.Get("Origin"); origin != "" {
				u, err := url.Parse(origin)
				if err != nil || !strings.EqualFold(u.Host, r.Host) {
					writeError(w, http.StatusForbidden, "forbidden", "cross-origin request")
					return
				}
			}
		}
		next.ServeHTTP(w, r)
	})
}

// JSON shapes of the API.
type (
	userJSON struct {
		ID    int64  `json:"id"`
		Email string `json:"email"`
		Name  string `json:"name"`
	}
	documentJSON struct {
		ID    string `json:"id"`
		Title string `json:"title"`
		Role  string `json:"role"`
	}
	versionJSON struct {
		ID        int64     `json:"id"`
		Name      string    `json:"name"`
		Seq       uint64    `json:"seq"`
		Author    string    `json:"author"`
		CreatedAt time.Time `json:"createdAt"`
		Text      *string   `json:"text,omitempty"` // only when one version is requested
	}
	linkJSON struct {
		ID        int64     `json:"id"`
		Role      string    `json:"role"`
		CreatedAt time.Time `json:"createdAt"`
		Token     string    `json:"token,omitempty"` // only in the response that creates the link
	}
)

func toUser(u auth.User) userJSON { return userJSON{ID: u.ID, Email: u.Email, Name: u.Name} }

func toDocument(d auth.Document) documentJSON {
	return documentJSON{ID: d.ID, Title: d.Title, Role: string(d.Role)}
}

func (a *api) register(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Email, Name, Password string
	}
	if !readJSON(w, r, &req) {
		return
	}
	user, token, err := a.svc.Register(r.Context(), req.Email, req.Name, req.Password)
	if err != nil {
		a.fail(w, err)
		return
	}
	a.setSession(w, token)
	writeJSON(w, http.StatusCreated, toUser(user))
}

func (a *api) login(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Email, Password string
	}
	if !readJSON(w, r, &req) {
		return
	}
	user, token, err := a.svc.Login(r.Context(), req.Email, req.Password)
	if err != nil {
		a.fail(w, err)
		return
	}
	a.setSession(w, token)
	writeJSON(w, http.StatusOK, toUser(user))
}

func (a *api) logout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(auth.SessionCookie); err == nil {
		if err := a.svc.Logout(r.Context(), c.Value); err != nil {
			a.fail(w, err)
			return
		}
	}
	http.SetCookie(w, a.cookie("", -1))
	w.WriteHeader(http.StatusNoContent)
}

func (a *api) me(w http.ResponseWriter, _ *http.Request, user auth.User) {
	writeJSON(w, http.StatusOK, toUser(user))
}

func (a *api) documents(w http.ResponseWriter, r *http.Request, user auth.User) {
	docs, err := a.svc.Documents(r.Context(), user.ID)
	if err != nil {
		a.fail(w, err)
		return
	}
	out := make([]documentJSON, 0, len(docs))
	for _, d := range docs {
		out = append(out, toDocument(d))
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *api) createDocument(w http.ResponseWriter, r *http.Request, user auth.User) {
	var req struct{ Title string }
	if !readJSON(w, r, &req) {
		return
	}
	d, err := a.svc.CreateDocument(r.Context(), user.ID, req.Title)
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, toDocument(d))
}

func (a *api) document(w http.ResponseWriter, r *http.Request, user auth.User) {
	d, err := a.svc.Document(r.Context(), r.PathValue("id"), user.ID)
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, toDocument(d))
}

func (a *api) links(w http.ResponseWriter, r *http.Request, user auth.User) {
	links, err := a.svc.Links(r.Context(), r.PathValue("id"), user.ID)
	if err != nil {
		a.fail(w, err)
		return
	}
	out := make([]linkJSON, 0, len(links))
	for _, l := range links {
		out = append(out, linkJSON{ID: l.ID, Role: string(l.Role), CreatedAt: l.CreatedAt})
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *api) createLink(w http.ResponseWriter, r *http.Request, user auth.User) {
	var req struct{ Role string }
	if !readJSON(w, r, &req) {
		return
	}
	role, err := auth.ParseRole(req.Role)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	l, token, err := a.svc.CreateLink(r.Context(), r.PathValue("id"), user.ID, role)
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, linkJSON{ID: l.ID, Role: string(l.Role), CreatedAt: l.CreatedAt, Token: token})
}

func (a *api) revokeLink(w http.ResponseWriter, r *http.Request, user auth.User) {
	linkID, err := strconv.ParseInt(r.PathValue("linkID"), 10, 64)
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found", "no such link")
		return
	}
	docID := r.PathValue("id")
	affected, err := a.svc.RevokeLink(r.Context(), docID, user.ID, linkID)
	if err != nil {
		a.fail(w, err)
		return
	}
	// Those who joined through the link may be editing right now with the
	// role it gave them: disconnect them, so that they are checked again.
	if err := a.versions.AccessChanged(r.Context(), docID, affected); err != nil {
		a.logger.Warn("cannot re-check connections after revoking a link", "doc", docID, "err", err)
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *api) join(w http.ResponseWriter, r *http.Request, user auth.User) {
	d, err := a.svc.JoinByLink(r.Context(), r.PathValue("token"), user.ID)
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, toDocument(d))
}

func toVersion(v doc.Version) versionJSON {
	return versionJSON{ID: v.ID, Name: v.Name, Seq: v.Seq, Author: v.AuthorName, CreatedAt: v.CreatedAt}
}

func (a *api) listVersions(w http.ResponseWriter, r *http.Request, user auth.User) {
	d, ok := a.access(w, r, user, false)
	if !ok {
		return
	}
	versions, err := a.versions.Versions(r.Context(), d.ID)
	if err != nil {
		a.fail(w, err)
		return
	}
	out := make([]versionJSON, 0, len(versions))
	for _, v := range versions {
		out = append(out, toVersion(v))
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *api) saveVersion(w http.ResponseWriter, r *http.Request, user auth.User) {
	var req struct{ Name string }
	if !readJSON(w, r, &req) {
		return
	}
	d, ok := a.access(w, r, user, true)
	if !ok {
		return
	}
	v, err := a.versions.SaveVersion(r.Context(), d.ID, user.ID, req.Name)
	if err != nil {
		a.fail(w, err)
		return
	}
	v.AuthorName = user.Name
	writeJSON(w, http.StatusCreated, toVersion(v))
}

func (a *api) version(w http.ResponseWriter, r *http.Request, user auth.User) {
	d, ok := a.access(w, r, user, false)
	if !ok {
		return
	}
	id, ok := versionID(w, r)
	if !ok {
		return
	}
	v, text, err := a.versions.VersionText(r.Context(), d.ID, id)
	if err != nil {
		a.fail(w, err)
		return
	}
	out := toVersion(v)
	out.Text = &text
	writeJSON(w, http.StatusOK, out)
}

func (a *api) restoreVersion(w http.ResponseWriter, r *http.Request, user auth.User) {
	d, ok := a.access(w, r, user, true)
	if !ok {
		return
	}
	id, ok := versionID(w, r)
	if !ok {
		return
	}
	seq, err := a.versions.RestoreVersion(r.Context(), d.ID, user.ID, id)
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]uint64{"seq": seq})
}

// access answers 404 if the user has no role in the document and 403 if
// edit is required and the role does not allow it.
func (a *api) access(w http.ResponseWriter, r *http.Request, user auth.User, edit bool) (auth.Document, bool) {
	d, err := a.svc.Document(r.Context(), r.PathValue("id"), user.ID)
	if err != nil {
		a.fail(w, err)
		return auth.Document{}, false
	}
	if edit && !d.Role.CanEdit() {
		a.fail(w, auth.ErrForbidden)
		return auth.Document{}, false
	}
	return d, true
}

func versionID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("versionID"), 10, 64)
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found", "no such version")
		return 0, false
	}
	return id, true
}

// withUser runs h for a logged-in user and answers 401 otherwise.
func (a *api) withUser(h func(http.ResponseWriter, *http.Request, auth.User)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var token string
		if c, err := r.Cookie(auth.SessionCookie); err == nil {
			token = c.Value
		}
		user, err := a.svc.UserBySession(r.Context(), token)
		if err != nil {
			a.fail(w, err)
			return
		}
		h(w, r, user)
	}
}

func (a *api) setSession(w http.ResponseWriter, token string) {
	http.SetCookie(w, a.cookie(token, int(a.opts.SessionTTL.Seconds())))
}

func (a *api) cookie(value string, maxAge int) *http.Cookie {
	return &http.Cookie{
		Name:     auth.SessionCookie,
		Value:    value,
		Path:     "/",
		MaxAge:   maxAge,
		HttpOnly: true, // not readable from JavaScript: an XSS cannot steal it
		Secure:   a.opts.CookieSecure,
		SameSite: http.SameSiteLaxMode,
	}
}

// fail maps service errors to HTTP statuses; anything unexpected is a 500
// whose details stay in the log.
func (a *api) fail(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, auth.ErrInvalidInput):
		writeError(w, http.StatusBadRequest, "bad_request", strings.TrimPrefix(err.Error(), auth.ErrInvalidInput.Error()+": "))
	case errors.Is(err, auth.ErrUnauthenticated):
		writeError(w, http.StatusUnauthorized, "unauthorized", "log in first")
	case errors.Is(err, auth.ErrInvalidCredentials):
		writeError(w, http.StatusUnauthorized, "invalid_credentials", "wrong e-mail or password")
	case errors.Is(err, auth.ErrForbidden):
		writeError(w, http.StatusForbidden, "forbidden", "your role does not allow this")
	case errors.Is(err, auth.ErrNotFound), errors.Is(err, doc.ErrNoVersion):
		writeError(w, http.StatusNotFound, "not_found", "not found")
	case errors.Is(err, doc.ErrInvalidName):
		writeError(w, http.StatusBadRequest, "bad_request", "version name must be 1-100 characters")
	case errors.Is(err, doc.ErrHubClosed):
		writeError(w, http.StatusServiceUnavailable, "unavailable", "server is shutting down")
	case errors.Is(err, auth.ErrEmailTaken):
		writeError(w, http.StatusConflict, "email_taken", "this e-mail is already registered")
	case errors.Is(err, context.Canceled):
		// The client went away; nobody reads the answer.
	default:
		a.logger.Error("request failed", "err", err)
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
	}
}

// readJSON decodes a small JSON body. Requiring application/json also means
// a plain HTML form on another site cannot send the request.
func readJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	mediaType, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if mediaType != "application/json" {
		writeError(w, http.StatusUnsupportedMediaType, "bad_request", "body must be application/json")
		return false
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxBodySize)
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "malformed JSON body")
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	// A write error means the client is gone; there is nobody to report it to.
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]string{"error": code, "message": message})
}
