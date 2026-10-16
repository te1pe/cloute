package doc_test

import (
	"errors"
	"testing"

	"cloute/internal/auth"
	"cloute/internal/crdt"
	"cloute/internal/doc"
	"cloute/internal/doc/doctest"
)

func TestPresence(t *testing.T) {
	r := newRegistry(t, doctest.NewMemStore(), testConfig)
	h, alice, wa := rejoin(t, r, "doc", doc.JoinRequest{UserID: 1, UserName: "Алиса"})
	aliceDoc := restore(t, wa)
	typeText(t, h, alice, aliceDoc, "abc")
	_, bob, _ := rejoin(t, r, "doc", doc.JoinRequest{UserID: 2, UserName: "Боб"})

	// Alice selects "b": from after a to after b.
	a, b := aliceDoc.Snapshot()[0].ID, aliceDoc.Snapshot()[1].ID
	if err := h.SetCursor(t.Context(), alice, doc.Cursor{Anchor: a, Head: b}); err != nil {
		t.Fatal(err)
	}
	p := next(t, bob).(doc.Presence)
	if p.Replica != alice.Replica() || p.UserName != "Алиса" || p.Cursor == nil || *p.Cursor != (doc.Cursor{Anchor: a, Head: b}) {
		t.Fatalf("bob got %+v", p)
	}

	// A cursor at a character the hub does not know is ignored.
	if err := h.SetCursor(t.Context(), alice, doc.Cursor{Anchor: crdt.ID{Lamport: 99, Replica: 9}}); err != nil {
		t.Fatal(err)
	}

	// A newcomer gets the existing cursors right after the welcome.
	_, carol, _ := rejoin(t, r, "doc", doc.JoinRequest{UserID: 3, UserName: "Кэрол"})
	if p := next(t, carol).(doc.Presence); p.Replica != alice.Replica() || *p.Cursor != (doc.Cursor{Anchor: a, Head: b}) {
		t.Fatalf("newcomer got %+v", p)
	}

	// When Alice leaves, the others are told to remove her cursor.
	h.Leave(alice)
	for _, c := range []*doc.Client{bob, carol} {
		if p := next(t, c).(doc.Presence); p.Replica != alice.Replica() || p.Cursor != nil {
			t.Fatalf("after leave got %+v", p)
		}
	}
}

func TestComments(t *testing.T) {
	store := doctest.NewMemStore()
	r := newRegistry(t, store, testConfig)
	h, owner, w := rejoin(t, r, "doc", doc.JoinRequest{UserID: 1, UserName: "Студент", Role: auth.RoleOwner})
	text := restore(t, w)
	typeText(t, h, owner, text, "Введение")
	_, teacher, _ := rejoin(t, r, "doc", doc.JoinRequest{UserID: 2, UserName: "Преподаватель", Role: auth.RoleCommenter})

	snap := text.Snapshot()
	from, to := snap[0].ID, snap[3].ID // "Введ"
	if err := h.AddComment(t.Context(), teacher, from, to, "  уточнить  "); err != nil {
		t.Fatal(err)
	}
	for _, c := range []*doc.Client{owner, teacher} {
		got := next(t, c).(doc.CommentChanged).Comment
		if got.ID == 0 || got.From != from || got.To != to || got.Body != "уточнить" || got.AuthorName != "Преподаватель" || got.Resolved {
			t.Fatalf("comment = %+v", got)
		}
	}

	// A newcomer gets the unresolved comments after the welcome.
	_, late, _ := rejoin(t, r, "doc", doc.JoinRequest{UserID: 3, Role: auth.RoleViewer})
	if list := next(t, late).(doc.Comments).List; len(list) != 1 || list[0].Body != "уточнить" {
		t.Fatalf("newcomer got %+v", list)
	}

	// The owner (an editor) may resolve someone else's comment.
	if err := h.ResolveComment(t.Context(), owner, 1); err != nil {
		t.Fatal(err)
	}
	if got := next(t, teacher).(doc.CommentChanged).Comment; got.ID != 1 || !got.Resolved {
		t.Fatalf("resolve = %+v", got)
	}
	next(t, owner)
	next(t, late)
	// Resolving it again is a no-op: someone may have been quicker.
	if err := h.ResolveComment(t.Context(), teacher, 1); err != nil {
		t.Fatal(err)
	}
	if list, _ := store.Comments(t.Context(), "doc"); len(list) != 0 {
		t.Fatalf("store still has %d unresolved comments", len(list))
	}
}

func TestCommentRules(t *testing.T) {
	setup := func(t *testing.T) (*doc.Registry, *doc.Hub, crdt.ID, crdt.ID) {
		r := newRegistry(t, doctest.NewMemStore(), testConfig)
		h, c, w := join(t, r, "doc")
		d := restore(t, w)
		typeText(t, h, c, d, "ab")
		return r, h, d.Snapshot()[0].ID, d.Snapshot()[1].ID
	}
	expectDrop := func(t *testing.T, c *doc.Client, target error) {
		t.Helper()
		if err := waitClosed(t, c); !errors.Is(err, target) {
			t.Fatalf("dropped with %v, want %v", err, target)
		}
	}

	t.Run("viewer cannot comment", func(t *testing.T) {
		r, h, a, b := setup(t)
		_, v, _ := rejoin(t, r, "doc", doc.JoinRequest{UserID: 5, Role: auth.RoleViewer})
		_ = h.AddComment(t.Context(), v, a, b, "x")
		expectDrop(t, v, doc.ErrForbidden)
	})
	t.Run("range must be in document order", func(t *testing.T) {
		r, h, a, b := setup(t)
		_, c, _ := rejoin(t, r, "doc", doc.JoinRequest{UserID: 5, Role: auth.RoleCommenter})
		_ = h.AddComment(t.Context(), c, b, a, "x")
		expectDrop(t, c, doc.ErrInvalidOp)
	})
	t.Run("range must be known characters", func(t *testing.T) {
		r, h, a, _ := setup(t)
		_, c, _ := rejoin(t, r, "doc", doc.JoinRequest{UserID: 5, Role: auth.RoleCommenter})
		_ = h.AddComment(t.Context(), c, a, crdt.ID{Lamport: 50, Replica: 50}, "x")
		expectDrop(t, c, doc.ErrInvalidOp)
	})
	t.Run("body must not be empty", func(t *testing.T) {
		r, h, a, b := setup(t)
		_, c, _ := rejoin(t, r, "doc", doc.JoinRequest{UserID: 5, Role: auth.RoleCommenter})
		_ = h.AddComment(t.Context(), c, a, b, "   ")
		expectDrop(t, c, doc.ErrInvalidOp)
	})
	t.Run("commenter cannot resolve others' comments", func(t *testing.T) {
		r, h, a, b := setup(t)
		_, author, _ := rejoin(t, r, "doc", doc.JoinRequest{UserID: 5, Role: auth.RoleCommenter})
		_ = h.AddComment(t.Context(), author, a, b, "mine")
		next(t, author)
		_, other, _ := rejoin(t, r, "doc", doc.JoinRequest{UserID: 6, Role: auth.RoleCommenter})
		next(t, other) // the comments list
		_ = h.ResolveComment(t.Context(), other, 1)
		expectDrop(t, other, doc.ErrForbidden)

		// The author may.
		_ = h.ResolveComment(t.Context(), author, 1)
		if got := next(t, author).(doc.CommentChanged).Comment; !got.Resolved {
			t.Fatalf("author's resolve = %+v", got)
		}
	})
}

func TestCommentsSurviveReload(t *testing.T) {
	store := doctest.NewMemStore()
	r1 := newRegistry(t, store, testConfig)
	h, c, w := join(t, r1, "doc")
	d := restore(t, w)
	typeText(t, h, c, d, "ab")
	_ = h.AddComment(t.Context(), c, d.Snapshot()[0].ID, d.Snapshot()[1].ID, "note")
	next(t, c)
	r1.Close()

	r2 := newRegistry(t, store, testConfig)
	_, c2, _ := join(t, r2, "doc")
	if list := next(t, c2).(doc.Comments).List; len(list) != 1 || list[0].Body != "note" {
		t.Fatalf("after reload: %+v", list)
	}
}

func TestAccessChangedDisconnectsTheUser(t *testing.T) {
	r := newRegistry(t, doctest.NewMemStore(), testConfig)
	_, alice, _ := rejoin(t, r, "doc", doc.JoinRequest{UserID: 1})
	_, bobTab1, _ := rejoin(t, r, "doc", doc.JoinRequest{UserID: 2})
	_, bobTab2, _ := rejoin(t, r, "doc", doc.JoinRequest{UserID: 2})

	if err := r.AccessChanged(t.Context(), "doc", []int64{2}); err != nil {
		t.Fatal(err)
	}
	for _, c := range []*doc.Client{bobTab1, bobTab2} {
		if err := waitClosed(t, c); !errors.Is(err, doc.ErrAccessChanged) {
			t.Fatalf("bob's tab dropped with %v, want ErrAccessChanged", err)
		}
	}
	select {
	case msg, ok := <-alice.Messages():
		if !ok {
			t.Fatal("alice was disconnected too")
		}
		_ = msg
	default:
	}

	// A document nobody has open is not loaded for this.
	if err := r.AccessChanged(t.Context(), "closed-doc", []int64{2}); err != nil {
		t.Fatal(err)
	}
}
