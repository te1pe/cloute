package doc

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"unicode/utf8"
)

var ErrInvalidName = errors.New("doc: version name must be 1-100 characters")

const joinAttempts = 3

type Registry struct {
	store  Store
	cfg    Config
	logger *slog.Logger
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	mu     sync.Mutex
	hubs   map[string]*Hub
	closed bool
}

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

func (r *Registry) RestoreVersion(ctx context.Context, docID string, userID, versionID int64) (uint64, error) {
	var seq uint64
	_, err := r.withHub(docID, func(h *Hub) error {
		var err error
		seq, err = h.Restore(ctx, userID, versionID)
		return err
	})
	return seq, err
}

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

func (r *Registry) Versions(ctx context.Context, docID string) ([]Version, error) {
	return r.store.Versions(ctx, docID)
}

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
	}
	return nil, err
}

func (r *Registry) hub(docID string) *Hub {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.closed {
		// не гонимся с wg.Wait
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
	documentsLoaded.Dec()
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.hubs[docID] == h {
		delete(r.hubs, docID)
	}
}

func (r *Registry) Close() {
	r.mu.Lock()
	r.closed = true
	r.mu.Unlock()

	r.cancel()
	r.wg.Wait()
}
