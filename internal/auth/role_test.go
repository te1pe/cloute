package auth

import "testing"

func TestRolePermissions(t *testing.T) {
	tests := []struct {
		role                 Role
		edit, comment, share bool
	}{
		{RoleOwner, true, true, true},
		{RoleEditor, true, true, false},
		{RoleCommenter, false, true, false},
		{RoleViewer, false, false, false},
		{Role(""), false, false, false},
	}
	for _, tt := range tests {
		if tt.role.CanEdit() != tt.edit || tt.role.CanComment() != tt.comment || tt.role.CanShare() != tt.share {
			t.Errorf("%q: edit %v comment %v share %v", tt.role, tt.role.CanEdit(), tt.role.CanComment(), tt.role.CanShare())
		}
	}
	if !RoleEditor.AtLeast(RoleCommenter) || RoleViewer.AtLeast(RoleCommenter) || !RoleOwner.AtLeast(RoleOwner) {
		t.Error("AtLeast does not follow owner > editor > commenter > viewer")
	}
	if _, err := ParseRole("admin"); err == nil {
		t.Error("ParseRole accepted an unknown role")
	}
}
