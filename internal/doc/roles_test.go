package doc_test

import (
	"errors"
	"testing"

	"cloute/internal/auth"
	"cloute/internal/crdt"
	"cloute/internal/doc"
	"cloute/internal/doc/doctest"
)

// TestReadOnlyRolesCannotEdit is the stage 5 acceptance test at hub level:
// operations from a viewer or a commenter are refused, nothing is stored
// or broadcast, and editors are not affected.
func TestReadOnlyRolesCannotEdit(t *testing.T) {
	for _, role := range []auth.Role{auth.RoleViewer, auth.RoleCommenter} {
		t.Run(string(role), func(t *testing.T) {
			store := doctest.NewMemStore()
			r := newRegistry(t, store, testConfig)
			_, editor, _ := join(t, r, "doc")
			h, reader, w := rejoin(t, r, "doc", doc.JoinRequest{UserID: 2, Role: role})
			if w.Role != role {
				t.Fatalf("welcome role = %q, want %q", w.Role, role)
			}

			op, _ := restore(t, w).InsertAt(0, 'x')
			submit(t, h, reader, op)
			if err := waitClosed(t, reader); !errors.Is(err, doc.ErrForbidden) {
				t.Fatalf("%s dropped with %v, want ErrForbidden", role, err)
			}
			if store.Seq("doc") != 0 {
				t.Fatalf("stored %d ops from a %s", store.Seq("doc"), role)
			}
			select {
			case msg := <-editor.Messages():
				t.Fatalf("editor got %+v", msg)
			default:
			}
		})
	}
}

func TestReadOnlyRolesCannotSendPending(t *testing.T) {
	store := doctest.NewMemStore()
	r := newRegistry(t, store, testConfig)
	_, viewer, w := rejoin(t, r, "doc", doc.JoinRequest{UserID: 2, Role: auth.RoleViewer})
	op, _ := restore(t, w).InsertAt(0, 'x')

	_, again, _ := rejoin(t, r, "doc", doc.JoinRequest{
		UserID: 2, Role: auth.RoleViewer, Replica: viewer.Replica(), LastSeq: w.Seq, Pending: []crdt.Op{op},
	})
	if err := waitClosed(t, again); !errors.Is(err, doc.ErrForbidden) {
		t.Fatalf("viewer with pending ops dropped with %v, want ErrForbidden", err)
	}
	if store.Seq("doc") != 0 {
		t.Fatal("pending ops of a viewer were stored")
	}
}

func TestOwnerAndEditorCanEdit(t *testing.T) {
	for _, role := range []auth.Role{auth.RoleOwner, auth.RoleEditor} {
		t.Run(string(role), func(t *testing.T) {
			r := newRegistry(t, doctest.NewMemStore(), testConfig)
			h, c, w := rejoin(t, r, "doc", doc.JoinRequest{UserID: 3, Role: role})
			typeText(t, h, c, restore(t, w), "ok")
		})
	}
}
