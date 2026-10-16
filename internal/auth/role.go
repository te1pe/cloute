// Package auth manages users, sessions and per-document roles.
package auth

import "fmt"

// SessionCookie is the name of the cookie that carries the session token.
const SessionCookie = "cloute_session"

// Role is what a user may do in one document.
type Role string

const (
	RoleOwner     Role = "owner"     // everything, including sharing
	RoleEditor    Role = "editor"    // edit the text
	RoleCommenter Role = "commenter" // read and comment
	RoleViewer    Role = "viewer"    // read only
)

// rank orders roles by what they allow.
var rank = map[Role]int{RoleViewer: 1, RoleCommenter: 2, RoleEditor: 3, RoleOwner: 4}

// ParseRole accepts the four role names.
func ParseRole(s string) (Role, error) {
	r := Role(s)
	if _, ok := rank[r]; !ok {
		return "", fmt.Errorf("unknown role %q", s)
	}
	return r, nil
}

// CanEdit reports whether the role may change the text.
func (r Role) CanEdit() bool { return r == RoleOwner || r == RoleEditor }

// CanComment reports whether the role may comment.
func (r Role) CanComment() bool { return r.CanEdit() || r == RoleCommenter }

// CanShare reports whether the role may manage access links.
func (r Role) CanShare() bool { return r == RoleOwner }

// AtLeast reports whether r allows everything other allows.
func (r Role) AtLeast(other Role) bool { return rank[r] >= rank[other] }
