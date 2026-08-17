package httpapi

import (
	"context"
	"testing"

	"github.com/EsDmitrii/kconmon-ng/internal/console/authz"
)

/*
A binding applies to a subject of the KIND it was written for, and to no other.

The resolver used to match on the subject id alone, so a 'user' binding whose subject_id happened to
be an API token's UUID handed that token the role. The RBAC API refuses to create a 'token' binding
outright — but the same effect was reachable through 'user', which it does allow.
*/

type kindRecordingResolver struct {
	sawKind string
	roles   []string
}

func (r *kindRecordingResolver) RolesFor(_ context.Context, s authz.Subject) ([]string, error) { //nolint:gocritic // matches the RoleResolver seam
	r.sawKind = string(s.Kind)
	return r.roles, nil
}

func TestResolveRolesCarriesTheSubjectKind(t *testing.T) {
	for _, kind := range []authz.SubjectKind{authz.SubjectUser, authz.SubjectToken} {
		resolver := &kindRecordingResolver{roles: []string{"viewer"}}
		s := &Server{roles: resolver, cfg: authTestConfig("local")}

		got := s.resolveRoles(context.Background(), authz.Subject{Kind: kind, ID: "same-id"})

		if resolver.sawKind != string(kind) {
			t.Errorf("resolver saw kind %q for a %q subject: a binding must not cross kinds", resolver.sawKind, kind)
		}
		if len(got.Roles) != 1 || got.Roles[0] != "viewer" {
			t.Errorf("roles = %v, want the resolved one", got.Roles)
		}
	}
}
