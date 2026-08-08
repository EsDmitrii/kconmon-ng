package authz_test

import (
	"sync"
	"testing"

	"github.com/EsDmitrii/kconmon-ng/internal/console/authz"
)

// TestViewerCoversTheM1M2Surface is the contract that keeps anonymous mode
// working: it lists every permission the M1/M2 routes require and asserts
// viewer holds all of them. If a later task adds a permission to an existing
// route without updating the viewer role, this test tells them.
func TestViewerCoversTheM1M2Surface(t *testing.T) {
	t.Parallel()

	m1m2Surface := []authz.Permission{
		authz.PermTopologyRead,
		authz.PermMatrixRead,
		authz.PermEventsRead,
		authz.PermPromQLQuery,
		authz.PermRunsRead,
	}

	policy := authz.NewPolicy(nil)
	viewer := authz.Subject{Kind: authz.SubjectAnonymous, ID: "anonymous", Roles: []string{"viewer"}}

	for _, perm := range m1m2Surface {
		if !policy.Can(viewer, perm) {
			t.Errorf("viewer denied M1/M2 permission %q — this breaks auth.mode=anonymous", perm)
		}
	}
}

// m4Permissions is the set M4 added for the targets/checks/schedules surface.
// Listed here once, in AllPermissions order, and reused by every grant
// assertion below so the role table is pinned from a single place.
var m4Permissions = []authz.Permission{
	authz.PermTargetsRead,
	authz.PermTargetsWrite,
	authz.PermChecksRead,
	authz.PermChecksWrite,
	authz.PermSchedulesWrite,
}

// TestOperatorAddsWriteAuthorityOverViewer pins operator - viewer as an
// EXACT set, not a superset check: runs:create plus the five M4 permissions,
// nothing more. Widening operator silently is exactly what this refuses.
func TestOperatorAddsWriteAuthorityOverViewer(t *testing.T) {
	t.Parallel()

	policy := authz.NewPolicy(nil)
	viewer := authz.Subject{Kind: authz.SubjectUser, ID: "v", Roles: []string{"viewer"}}
	operator := authz.Subject{Kind: authz.SubjectUser, ID: "o", Roles: []string{"operator"}}

	viewerPerms := policy.PermissionsFor(viewer)
	operatorPerms := policy.PermissionsFor(operator)

	viewerSet := make(map[authz.Permission]struct{}, len(viewerPerms))
	for _, p := range viewerPerms {
		viewerSet[p] = struct{}{}
	}

	var diff []authz.Permission
	for _, p := range operatorPerms {
		if _, ok := viewerSet[p]; !ok {
			diff = append(diff, p)
		}
	}

	// PermissionsFor returns AllPermissions order, so this expectation is
	// order-stable without sorting. M5: annotations:write is the one M5
	// permission operator holds that viewer does not (mtr:read and
	// annotations:read are telemetry and land in BOTH — Plan Decision 11).
	want := append([]authz.Permission{authz.PermRunsCreate}, m4Permissions...)
	want = append(want, authz.PermAnnotationsWrite)
	// M6: the two statement-class writes (webhooks:manage stays admin-only
	// and so is in NEITHER role's diff).
	want = append(want, authz.PermIncidentsWrite, authz.PermMaintenanceWrite)
	if len(diff) != len(want) {
		t.Fatalf("operator - viewer = %v, want %v", diff, want)
	}
	for i := range want {
		if diff[i] != want[i] {
			t.Fatalf("operator - viewer = %v, want %v", diff, want)
		}
	}

	for _, p := range viewerPerms {
		if !policy.Can(operator, p) {
			t.Errorf("operator lost viewer permission %q", p)
		}
	}
}

// TestM4PermissionsAreDeniedToViewerAndAlertEditor is Decision 3 as a test.
// viewer is the anonymous default (auth.mode=anonymous +
// auth.anonymous.role=viewer), so granting it targets:read would silently
// hand an unauthenticated console the fleet's probe configuration -- and
// targets:write would hand it the authority to point N agents at an
// arbitrary address. alert-editor is a placeholder for M7 alerting and has
// no business mutating targets either.
func TestM4PermissionsAreDeniedToViewerAndAlertEditor(t *testing.T) {
	t.Parallel()

	policy := authz.NewPolicy(nil)
	for _, role := range []string{"viewer", "alert-editor"} {
		s := authz.Subject{Kind: authz.SubjectUser, ID: "x", Roles: []string{role}}
		for _, perm := range m4Permissions {
			if policy.Can(s, perm) {
				t.Errorf("%s holds %q, want deny (M4 Decision 3)", role, perm)
			}
		}
	}
}

// TestM4PermissionsAreGrantedToOperatorAndAdmin is the other half of
// Decision 3.
func TestM4PermissionsAreGrantedToOperatorAndAdmin(t *testing.T) {
	t.Parallel()

	policy := authz.NewPolicy(nil)
	for _, role := range []string{"operator", "admin"} {
		s := authz.Subject{Kind: authz.SubjectUser, ID: "x", Roles: []string{role}}
		for _, perm := range m4Permissions {
			if !policy.Can(s, perm) {
				t.Errorf("%s denied %q, want grant", role, perm)
			}
		}
	}
}

// TestAdminHoldsEveryPermission pins AllPermissions against an explicit
// expected list (rather than the constant block itself) so that a new
// Permission constant added without a matching AllPermissions entry fails
// this test.
func TestAdminHoldsEveryPermission(t *testing.T) {
	t.Parallel()

	expected := []authz.Permission{
		authz.PermTopologyRead,
		authz.PermMatrixRead,
		authz.PermEventsRead,
		authz.PermPromQLQuery,
		authz.PermRunsRead,
		authz.PermRunsCreate,
		authz.PermAuditRead,
		authz.PermRBACManage,
		authz.PermTokensManage,
		authz.PermSettingsWrite,
		authz.PermTargetsRead,
		authz.PermTargetsWrite,
		authz.PermChecksRead,
		authz.PermChecksWrite,
		authz.PermSchedulesWrite,
		authz.PermMTRRead,
		authz.PermAnnotationsRead,
		authz.PermAnnotationsWrite,
		authz.PermIncidentsRead,
		authz.PermIncidentsWrite,
		authz.PermMaintenanceRead,
		authz.PermMaintenanceWrite,
		authz.PermWebhooksManage,
	}

	if len(authz.AllPermissions) != len(expected) {
		t.Fatalf("AllPermissions has %d entries, want %d: got %v", len(authz.AllPermissions), len(expected), authz.AllPermissions)
	}
	for i, perm := range expected {
		if authz.AllPermissions[i] != perm {
			t.Fatalf("AllPermissions[%d] = %q, want %q (a new Permission constant must be listed here)", i, authz.AllPermissions[i], perm)
		}
	}

	seen := make(map[authz.Permission]struct{}, len(authz.AllPermissions))
	for _, perm := range authz.AllPermissions {
		if _, dup := seen[perm]; dup {
			t.Fatalf("AllPermissions contains duplicate %q", perm)
		}
		seen[perm] = struct{}{}
	}

	policy := authz.NewPolicy(nil)
	admin := authz.Subject{Kind: authz.SubjectUser, ID: "root", Roles: []string{"admin"}}
	for _, perm := range authz.AllPermissions {
		if !policy.Can(admin, perm) {
			t.Errorf("admin lacks %q", perm)
		}
	}
}

func TestUnknownRoleGrantsNothing(t *testing.T) {
	t.Parallel()

	policy := authz.NewPolicy(nil)
	s := authz.Subject{Kind: authz.SubjectUser, ID: "u", Roles: []string{"typo"}}

	for _, perm := range authz.AllPermissions {
		if policy.Can(s, perm) {
			t.Fatalf("unknown role %q grants %q, want deny", "typo", perm)
		}
	}
	if got := policy.PermissionsFor(s); len(got) != 0 {
		t.Fatalf("PermissionsFor(unknown role) = %v, want empty", got)
	}
}

func TestCustomRoleOverlaysBuiltins(t *testing.T) {
	t.Parallel()

	policy := authz.NewPolicy(map[string][]authz.Permission{
		"auditor": {authz.PermAuditRead},
		// A custom role named like a built-in must not weaken it.
		"admin": {authz.PermTopologyRead},
	})

	auditor := authz.Subject{Kind: authz.SubjectUser, ID: "a", Roles: []string{"auditor"}}
	if !policy.Can(auditor, authz.PermAuditRead) {
		t.Fatal("custom role auditor should grant audit:read")
	}
	if policy.Can(auditor, authz.PermRunsCreate) {
		t.Fatal("custom role auditor should not grant unrelated permissions")
	}

	admin := authz.Subject{Kind: authz.SubjectUser, ID: "r", Roles: []string{"admin"}}
	for _, perm := range authz.AllPermissions {
		if !policy.Can(admin, perm) {
			t.Errorf("built-in admin weakened by colliding custom role: missing %q", perm)
		}
	}

	viewer := authz.Subject{Kind: authz.SubjectUser, ID: "v", Roles: []string{"viewer"}}
	if !policy.Can(viewer, authz.PermTopologyRead) {
		t.Fatal("built-in viewer should be untouched by unrelated custom roles")
	}
}

func TestSubjectWithNoRolesCanNothing(t *testing.T) {
	t.Parallel()

	policy := authz.NewPolicy(nil)
	s := authz.Subject{Kind: authz.SubjectUser, ID: "bob", DisplayName: "Bob"}

	for _, perm := range authz.AllPermissions {
		if policy.Can(s, perm) {
			t.Fatalf("authenticated subject with no roles bound grants %q, want deny", perm)
		}
	}
	if got := policy.PermissionsFor(s); len(got) != 0 {
		t.Fatalf("PermissionsFor(no roles) = %v, want empty", got)
	}
}

func TestCanIsPureAndConcurrencySafe(t *testing.T) {
	t.Parallel()

	policy := authz.NewPolicy(map[string][]authz.Permission{
		"auditor": {authz.PermAuditRead},
	})

	subjects := []authz.Subject{
		{Kind: authz.SubjectAnonymous, ID: "anonymous", Roles: []string{"viewer"}},
		{Kind: authz.SubjectUser, ID: "o", Roles: []string{"operator"}},
		{Kind: authz.SubjectUser, ID: "adm", Roles: []string{"admin"}},
		{Kind: authz.SubjectToken, ID: "t", Roles: []string{"auditor"}},
		{Kind: authz.SubjectUser, ID: "typo", Roles: []string{"typo"}},
		{Kind: authz.SubjectUser, ID: "unbound"},
	}

	var wg sync.WaitGroup
	for i := range 50 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			s := subjects[i%len(subjects)]
			for _, perm := range authz.AllPermissions {
				_ = policy.Can(s, perm)
			}
			_ = policy.PermissionsFor(s)
		}(i)
	}
	wg.Wait()
}

// TestIsBuiltinRole pins the exact name set internal/console/config relies on
// to validate auth.defaultRole.
func TestIsBuiltinRole(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		want bool
	}{
		{"viewer", true},
		{"operator", true},
		{"alert-editor", true},
		{"admin", true},
		{"auditor", false},
		{"", false},
		{"Viewer", false},
	} {
		if got := authz.IsBuiltinRole(tc.name); got != tc.want {
			t.Errorf("IsBuiltinRole(%q) = %v, want %v", tc.name, got, tc.want)
		}
	}
}
