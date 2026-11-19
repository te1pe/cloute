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
	ErrNotFound           = errors.New("auth: not found")
	ErrEmailTaken         = errors.New("auth: e-mail already registered")
	ErrInvalidCredentials = errors.New("auth: invalid e-mail or password")
	ErrUnauthenticated    = errors.New("auth: not logged in")
	ErrForbidden          = errors.New("auth: forbidden")
	ErrInvalidInput       = errors.New("auth: invalid input")
)

type User struct {
	ID    int64
	Email string
	Name  string
}

type Document struct {
	ID    string
	Title string
	Role  Role
}

type Link struct {
	ID        int64
	Role      Role
	CreatedAt time.Time
}

type Store interface {
	CreateUser(ctx context.Context, email, name string, passwordHash []byte) (User, error)
	UserByEmail(ctx context.Context, email string) (User, []byte, error)
	CreateSession(ctx context.Context, tokenHash []byte, userID int64, expires time.Time) error
	SessionUser(ctx context.Context, tokenHash []byte, now time.Time) (User, error)
	DeleteSession(ctx context.Context, tokenHash []byte) error

	CreateDocument(ctx context.Context, docID, title string, owner int64) error
	Document(ctx context.Context, docID string, userID int64) (Document, error)
	Documents(ctx context.Context, userID int64) ([]Document, error)

	CreateLink(ctx context.Context, tokenHash []byte, docID string, role Role, createdBy int64) (Link, error)
	Links(ctx context.Context, docID string) ([]Link, error)
	RevokeLink(ctx context.Context, docID string, linkID int64) ([]int64, error)
	JoinByLink(ctx context.Context, tokenHash []byte, userID int64) (Document, error)
}

type Config struct {
	BcryptCost int
	SessionTTL time.Duration
}

type Service struct {
	store Store
	cfg   Config
	now   func() time.Time
	dummy []byte
}

func NewService(store Store, cfg Config) (*Service, error) {
	dummy, err := bcrypt.GenerateFromPassword([]byte("timing-equalizer"), cfg.BcryptCost)
	if err != nil {
		return nil, err
	}
	return &Service{store: store, cfg: cfg, now: time.Now, dummy: dummy}, nil
}

func (s *Service) Register(ctx context.Context, email, name, password string) (User, string, error) {
	email, err := normalizeEmail(email)
	if err != nil {
		return User{}, "", err
	}
	name = strings.TrimSpace(name)
	if n := utf8.RuneCountInString(name); n < 1 || n > 100 {
		return User{}, "", fmt.Errorf("%w: name must be 1-100 characters", ErrInvalidInput)
	}
	if len(password) < 8 || len(password) > 72 {
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

func (s *Service) Login(ctx context.Context, email, password string) (User, string, error) {
	email, err := normalizeEmail(email)
	if err != nil {
		return User{}, "", ErrInvalidCredentials
	}
	user, hash, err := s.store.UserByEmail(ctx, email)
	switch {
	case errors.Is(err, ErrNotFound):
		// то же время, что и при неверном пароле
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

func (s *Service) CreateDocument(ctx context.Context, userID int64, title string) (Document, error) {
	title = strings.TrimSpace(title)
	if title == "" {
		title = "Без названия"
	}
	if utf8.RuneCountInString(title) > 200 {
		return Document{}, fmt.Errorf("%w: title must be at most 200 characters", ErrInvalidInput)
	}
	id := strings.ToLower(rand.Text())
	if err := s.store.CreateDocument(ctx, id, title, userID); err != nil {
		return Document{}, err
	}
	return Document{ID: id, Title: title, Role: RoleOwner}, nil
}

func (s *Service) Document(ctx context.Context, docID string, userID int64) (Document, error) {
	return s.store.Document(ctx, docID, userID)
}

func (s *Service) Documents(ctx context.Context, userID int64) ([]Document, error) {
	return s.store.Documents(ctx, userID)
}

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

func (s *Service) RevokeLink(ctx context.Context, docID string, userID, linkID int64) ([]int64, error) {
	if _, err := s.requireShare(ctx, docID, userID); err != nil {
		return nil, err
	}
	return s.store.RevokeLink(ctx, docID, linkID)
}

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

func newToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// в 256 битах соль не нужна
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
