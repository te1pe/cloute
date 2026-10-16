package doc

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"unicode/utf8"
)

// ErrInvalidName means a version name is empty or longer than 100 characters.
var ErrInvalidName = errors.New("doc: version name must be 1-100 characters")

// joinAttempts bounds retries when a hub unloads just as a client joins.
const joinAttempts = 3

// Registry runs one Hub per open document: it starts a hub on the first
// join, forgets it when the hub unloads, and stops all hubs on Close.
type Registry struct {
	store  Store
	cfg    Config
	logger *slog.Logger
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	mu     sync.Mutex // guards hubs and closed; document state belongs to hubs
	hubs   map[string]*Hub
	closed bool
}

// NewRegistry returns a registry whose hubs persist documents in store.
func NewRegistry(store Store, cfg Config, logger *slog.Logger) *Registry {
	ctx, cancel := context.WithCancel(context.Background())
	return &Registry{
		store:  store,
		cfg:    cfg,
		logger: logger,
		ctx:    ctx,
		cancel: cancel,
		hubs:   make(map[string]*Hub),
	}
}

// Join attaches a new client to the document, loading it if needed.
func (r *Registry) Join(ctx context.Context, docID string, req JoinRequest) (*Hub, *Client, error) {
	var c *Client
	h, err := r.withHub(docID, func(h *Hub) error {
		var err error
		c, err = h.Join(ctx, req)
		return err
	})
	if err != nil {
		return nil, nil, err
	}
	return h, c, nil
}

// SaveVersion stores the current state of the document as a named version.
func (r *Registry) SaveVersion(ctx context.Context, docID string, userID int64, name string) (Version, error) {
	name = strings.TrimSpace(name)
	if n := utf8.RuneCountInString(name); n < 1 || n > 100 {
		return Version{}, ErrInvalidName
	}
	var v Version
	_, err := r.withHub(docID, func(h *Hub) error {
		var err error
		v, err = h.SaveVersion(ctx, userID, name)
		return err
	})
	return v, err
}

// RestoreVersion makes the document's text that of the version, merging
// with concurrent edits, and returns the sequence number after it.
func (r *Registry) RestoreVersion(ctx context.Context, docID string, userID, versionID int64) (uint64, error) {
	var seq uint64
	_, err := r.withHub(docID, func(h *Hub) error {
		var err error
		seq, err = h.Restore(ctx, userID, versionID)
		return err
	})
	return seq, err
}

// AccessChanged disconnects the users' clients from the document if it is
// open, so that their roles are checked again. A closed document has no
// clients and is not loaded for this.
func (r *Registry) AccessChanged(ctx context.Context, docID string, userIDs []int64) error {
	if len(userIDs) == 0 {
		return nil
	}
	r.mu.Lock()
	h := r.hubs[docID]
	r.mu.Unlock()
	if h == nil {
		return nil
	}
	return h.AccessChanged(ctx, userIDs)
}

// Versions lists the document's versions; it does not need the hub.
func (r *Registry) Versions(ctx context.Context, docID string) ([]Version, error) {
	return r.store.Versions(ctx, docID)
}

// VersionText returns a version with its visible text.
func (r *Registry) VersionText(ctx context.Context, docID string, versionID int64) (Version, string, error) {
	v, state, err := r.store.Version(ctx, docID, versionID)
	if err != nil {
		return Version{}, "", err
	}
	var b strings.Builder
	for _, e := range state {
		if !e.Deleted {
			b.WriteRune(e.Char)
		}
	}
	return v, b.String(), nil
}

// withHub calls fn with the running hub of the document. If the hub
// unloads between the lookup and the call, it retries with a fresh one.
func (r *Registry) withHub(docID string, fn func(*Hub) error) (*Hub, error) {
	err := ErrHubClosed
	for range joinAttempts {
		h := r.hub(docID)
		if h == nil {
			return nil, ErrHubClosed
		}
		if err = fn(h); !errors.Is(err, ErrHubClosed) {
			return h, err
		}
		// The hub unloaded between lookup and call; it is already gone
		// from the map, so the next lookup starts a fresh one.
	}
	return nil, err
}

// hub returns the running hub of the document, starting it if needed, or
// nil after Close.
func (r *Registry) hub(docID string) *Hub {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.closed {
		// wg.Go must not race with wg.Wait in Close, so never start a
		// goroutine after Close.
		return nil
	}
	if h, ok := r.hubs[docID]; ok {
		return h
	}
	h := newHub(docID, r.store, r.cfg, r.logger)
	r.hubs[docID] = h
	documentsLoaded.Inc()
	r.wg.Go(func() { h.run(r.ctx, func() { r.forget(docID, h) }) })
	return h
}

func (r *Registry) forget(docID string, h *Hub) {
	documentsLoaded.Dec() // every started hub stops exactly once
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.hubs[docID] == h {
		delete(r.hubs, docID)
	}
}

// Close stops all hubs, which save a final snapshot and detach their
// clients with ErrHubClosed, and waits for them. It is safe to call more
// than once.
func (r *Registry) Close() {
	r.mu.Lock()
	r.closed = true
	r.mu.Unlock()

	r.cancel()
	r.wg.Wait()
}
