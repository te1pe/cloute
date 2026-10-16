package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"golang.org/x/crypto/bcrypt"
)

var (
	// ErrNotFound means the thing does not exist or the user may not know
	// that it exists (a document they have no role in).
	ErrNotFound = errors.New("auth: not found")
	// ErrEmailTaken means a user with this e-mail already exists.
	ErrEmailTaken = errors.New("auth: e-mail already registered")
	// ErrInvalidCredentials means a wrong e-mail or password; which one is
	// deliberately not told.
	ErrInvalidCredentials = errors.New("auth: invalid e-mail or password")
	// ErrUnauthenticated means no valid session.
	ErrUnauthenticated = errors.New("auth: not logged in")
	// ErrForbidden means the user's role does not allow the action.
	ErrForbidden = errors.New("auth: forbidden")
	// ErrInvalidInput wraps validation errors of user input.
	ErrInvalidInput = errors.New("auth: invalid input")
)

type User struct {
	ID    int64
	Email string
	Name  string
}

// Document is a document as seen by one user.
type Document struct {
	ID    string
	Title string
	Role  Role
}

// Link is an access link without its token (the token is shown only once,
// when the link is created).
type Link struct {
	ID        int64
	Role      Role
	CreatedAt time.Time
}

// Store persists users, sessions, documents and access. Implementations
// return ErrNotFound and ErrEmailTaken where documented.
type Store interface {
	CreateUser(ctx context.Context, email, name string, passwordHash []byte) (User, error) // ErrEmailTaken
	UserByEmail(ctx context.Context, email string) (User, []byte, error)                   // ErrNotFound
	CreateSession(ctx context.Context, tokenHash []byte, userID int64, expires time.Time) error
	SessionUser(ctx context.Context, tokenHash []byte, now time.Time) (User, error) // ErrNotFound
	DeleteSession(ctx context.Context, tokenHash []byte) error

	CreateDocument(ctx context.Context, docID, title string, owner int64) error
	Document(ctx context.Context, docID string, userID int64) (Document, error) // ErrNotFound without a role
	Documents(ctx context.Context, userID int64) ([]Document, error)

	CreateLink(ctx context.Context, tokenHash []byte, docID string, role Role, createdBy int64) (Link, error)
	Links(ctx context.Context, docID string) ([]Link, error)
	// RevokeLink revokes the link and returns the users who joined through
	// it: their role may have dropped or gone. ErrNotFound if not active.
	RevokeLink(ctx context.Context, docID string, linkID int64) ([]int64, error)
	// JoinByLink records that the user opened the link and returns the
	// document with the user's role: the highest one among the unrevoked
	// links they opened (or owner).
	JoinByLink(ctx context.Context, tokenHash []byte, userID int64) (Document, error) // ErrNotFound
}

// Config tunes the service.
type Config struct {
	BcryptCost int           // bcrypt.DefaultCost in production, bcrypt.MinCost in tests
	SessionTTL time.Duration // how long a login lasts
}

// Service implements the rules on top of a Store.
type Service struct {
	store Store
	cfg   Config
	now   func() time.Time
	dummy []byte // a hash to compare against when the e-mail is unknown
}

func NewService(store Store, cfg Config) (*Service, error) {
	dummy, err := bcrypt.GenerateFromPassword([]byte("timing-equalizer"), cfg.BcryptCost)
	if err != nil {
		return nil, err
	}
	return &Service{store: store, cfg: cfg, now: time.Now, dummy: dummy}, nil
}

// Register creates a user and logs them in. It returns the session token.
func (s *Service) Register(ctx context.Context, email, name, password string) (User, string, error) {
	email, err := normalizeEmail(email)
	if err != nil {
		return User{}, "", err
	}
	name = strings.TrimSpace(name)
	if n := utf8.RuneCountInString(name); n < 1 || n > 100 {
		return User{}, "", fmt.Errorf("%w: name must be 1-100 characters", ErrInvalidInput)
	}
	if len(password) < 8 || len(password) > 72 { // bcrypt uses at most 72 bytes
		return User{}, "", fmt.Errorf("%w: password must be 8-72 bytes", ErrInvalidInput)
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), s.cfg.BcryptCost)
	if err != nil {
		return User{}, "", fmt.Errorf("auth: hash password: %w", err)
	}
	user, err := s.store.CreateUser(ctx, email, name, hash)
	if err != nil {
		return User{}, "", err
	}
	token, err := s.newSession(ctx, user.ID)
	return user, token, err
}

// Login checks the password and starts a session. It returns the token.
func (s *Service) Login(ctx context.Context, email, password string) (User, string, error) {
	email, err := normalizeEmail(email)
	if err != nil {
		return User{}, "", ErrInvalidCredentials
	}
	user, hash, err := s.store.UserByEmail(ctx, email)
	switch {
	case errors.Is(err, ErrNotFound):
		// Spend the same time as for a wrong password, so that response
		// times do not reveal which e-mails are registered.
		_ = bcrypt.CompareHashAndPassword(s.dummy, []byte(password))
		return User{}, "", ErrInvalidCredentials
	case err != nil:
		return User{}, "", err
	}
	if bcrypt.CompareHashAndPassword(hash, []byte(password)) != nil {
		return User{}, "", ErrInvalidCredentials
	}
	token, err := s.newSession(ctx, user.ID)
	return user, token, err
}

func (s *Service) Logout(ctx context.Context, token string) error {
	return s.store.DeleteSession(ctx, hashToken(token))
}

// UserBySession returns the user of a valid, unexpired session.
func (s *Service) UserBySession(ctx context.Context, token string) (User, error) {
	if token == "" {
		return User{}, ErrUnauthenticated
	}
	user, err := s.store.SessionUser(ctx, hashToken(token), s.now())
	if errors.Is(err, ErrNotFound) {
		return User{}, ErrUnauthenticated
	}
	return user, err
}

// CreateDocument creates an empty document owned by the user.
func (s *Service) CreateDocument(ctx context.Context, userID int64, title string) (Document, error) {
	title = strings.TrimSpace(title)
	if title == "" {
		title = "Без названия"
	}
	if utf8.RuneCountInString(title) > 200 {
		return Document{}, fmt.Errorf("%w: title must be at most 200 characters", ErrInvalidInput)
	}
	id := strings.ToLower(rand.Text()) // 26 characters of [a-z2-7], about 130 random bits
	if err := s.store.CreateDocument(ctx, id, title, userID); err != nil {
		return Document{}, err
	}
	return Document{ID: id, Title: title, Role: RoleOwner}, nil
}

// Document returns the document with the user's role, or ErrNotFound if
// the user has no role in it.
func (s *Service) Document(ctx context.Context, docID string, userID int64) (Document, error) {
	return s.store.Document(ctx, docID, userID)
}

func (s *Service) Documents(ctx context.Context, userID int64) ([]Document, error) {
	return s.store.Documents(ctx, userID)
}

// CreateLink creates an access link; only the owner may. The token is
// returned once and never stored in clear.
func (s *Service) CreateLink(ctx context.Context, docID string, userID int64, role Role) (Link, string, error) {
	if _, err := s.requireShare(ctx, docID, userID); err != nil {
		return Link{}, "", err
	}
	if role == RoleOwner || !role.AtLeast(RoleViewer) {
		return Link{}, "", fmt.Errorf("%w: a link gives editor, commenter or viewer", ErrInvalidInput)
	}
	token, err := newToken()
	if err != nil {
		return Link{}, "", err
	}
	link, err := s.store.CreateLink(ctx, hashToken(token), docID, role, userID)
	return link, token, err
}

func (s *Service) Links(ctx context.Context, docID string, userID int64) ([]Link, error) {
	if _, err := s.requireShare(ctx, docID, userID); err != nil {
		return nil, err
	}
	return s.store.Links(ctx, docID)
}

// RevokeLink revokes an access link; only the owner may. The role each user
// got from it ends at once. It returns those users, whose open connections
// must be re-checked.
func (s *Service) RevokeLink(ctx context.Context, docID string, userID, linkID int64) ([]int64, error) {
	if _, err := s.requireShare(ctx, docID, userID); err != nil {
		return nil, err
	}
	return s.store.RevokeLink(ctx, docID, linkID)
}

// JoinByLink records that the user opened the link and returns the
// document with the user's resulting role (a weaker link never lowers it).
func (s *Service) JoinByLink(ctx context.Context, token string, userID int64) (Document, error) {
	if token == "" {
		return Document{}, ErrNotFound
	}
	return s.store.JoinByLink(ctx, hashToken(token), userID)
}

func (s *Service) requireShare(ctx context.Context, docID string, userID int64) (Document, error) {
	d, err := s.store.Document(ctx, docID, userID)
	if err != nil {
		return Document{}, err
	}
	if !d.Role.CanShare() {
		return Document{}, ErrForbidden
	}
	return d, nil
}

func (s *Service) newSession(ctx context.Context, userID int64) (string, error) {
	token, err := newToken()
	if err != nil {
		return "", err
	}
	if err := s.store.CreateSession(ctx, hashToken(token), userID, s.now().Add(s.cfg.SessionTTL)); err != nil {
		return "", err
	}
	return token, nil
}

// newToken returns 256 random bits, URL-safe.
func newToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// hashToken is what the database stores instead of a token. A plain hash is
// enough (unlike for passwords): the token has 256 random bits, so there is
// nothing to brute-force.
func hashToken(token string) []byte {
	h := sha256.Sum256([]byte(token))
	return h[:]
}

func normalizeEmail(email string) (string, error) {
	email = strings.ToLower(strings.TrimSpace(email))
	at := strings.IndexByte(email, '@')
	if at < 1 || at == len(email)-1 || len(email) > 254 || strings.ContainsAny(email, " \t\r\n") {
		return "", fmt.Errorf("%w: invalid e-mail", ErrInvalidInput)
	}
	return email, nil
}
