package auth

import "fmt"

const SessionCookie = "cloute_session"

type Role string

const (
	RoleOwner     Role = "owner"
	RoleEditor    Role = "editor"
	RoleCommenter Role = "commenter"
	RoleViewer    Role = "viewer"
)

var rank = map[Role]int{RoleViewer: 1, RoleCommenter: 2, RoleEditor: 3, RoleOwner: 4}

func ParseRole(s string) (Role, error) {
	r := Role(s)
	if _, ok := rank[r]; !ok {
		return "", fmt.Errorf("unknown role %q", s)
	}
	return r, nil
}

func (r Role) CanEdit() bool { return r == RoleOwner || r == RoleEditor }

func (r Role) CanComment() bool { return r.CanEdit() || r == RoleCommenter }

func (r Role) CanShare() bool { return r == RoleOwner }

func (r Role) AtLeast(other Role) bool { return rank[r] >= rank[other] }
