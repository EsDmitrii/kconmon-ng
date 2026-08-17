package httpapi

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/EsDmitrii/kconmon-ng/internal/console/authz"
	"github.com/EsDmitrii/kconmon-ng/internal/console/ws"
)

/*
The socket serves the same snapshots the REST routes do, so it must ask the same permission. It used
to ask one question for the whole allowlist -- events:read -- which let a custom role read topology
and matrix over /ws after GET /api/v1/topology had answered 403, with no audit row for the read.
*/

func TestWSTopicPermissionMirrorsTheRESTSplit(t *testing.T) {
	tests := map[string]authz.Permission{
		ws.TopicLive:            authz.PermEventsRead,
		ws.TopicTopology:        authz.PermTopologyRead,
		ws.MatrixTopic("tcp"):   authz.PermMatrixRead,
		ws.MatrixTopic("udp"):   authz.PermMatrixRead,
		ws.MatrixTopic("icmp"):  authz.PermMatrixRead,
		"matrix:something-new":  authz.PermMatrixRead,
		ws.RunTopic("run-1234"): "",
	}
	for topic, want := range tests {
		if got := wsTopicPermission(topic); got != want {
			t.Errorf("wsTopicPermission(%q) = %q, want %q", topic, got, want)
		}
	}
}

// The bypass itself: events:read alone must no longer open the fleet's inventory.
func TestWSEventsReadAloneCannotSubscribeToTopologyOrMatrix(t *testing.T) {
	policy := authz.NewPolicy(map[string][]authz.Permission{"feed-watcher": {authz.PermEventsRead}})
	s := &Server{policy: policy}
	authorize := s.wsTopicAuthorizer(subjectCell(authz.Subject{Kind: authz.SubjectUser, ID: "u1", Roles: []string{"feed-watcher"}}))

	if err := authorize(ws.TopicLive); err != nil {
		t.Fatalf("events:read refused the live feed it does hold: %v", err)
	}
	for _, topic := range []string{ws.TopicTopology, ws.MatrixTopic("tcp")} {
		if err := authorize(topic); err == nil {
			t.Errorf("events:read alone subscribed to %s; GET /api/v1/topology answers 403 for the same subject", topic)
		}
	}
}

/*
The symmetric half, which the old gate also got wrong: a subject entitled to the snapshot was refused
it for lack of a permission the snapshot does not need.
*/
func TestWSTopologyReadAloneMaySubscribeToTopology(t *testing.T) {
	policy := authz.NewPolicy(map[string][]authz.Permission{
		"map-reader": {authz.PermTopologyRead, authz.PermRunsRead},
	})
	s := &Server{policy: policy}
	authorize := s.wsTopicAuthorizer(subjectCell(authz.Subject{Kind: authz.SubjectUser, ID: "u1", Roles: []string{"map-reader"}}))

	if err := authorize(ws.TopicTopology); err != nil {
		t.Errorf("topology:read was refused the topology topic: %v", err)
	}
	if err := authorize(ws.TopicLive); err == nil {
		t.Error("a subject without events:read was given the live feed")
	}
	// run:{id} is covered by the upgrade gate itself and by nothing narrower.
	if err := authorize(ws.RunTopic("r1")); err != nil {
		t.Errorf("runs:read was refused its own run topic: %v", err)
	}
}

/*
The other half of the same round: no route read an unbounded body. encoding/json buffers the whole
top-level value before it unmarshals, so a single oversized POST -- to any route, including the
public login one, which decodes before it checks a credential -- walked the pod past its memory
limit and the kubelet killed it, taking every in-flight request and every socket on that replica.
*/
func TestOversizedBodyIsRefusedRatherThanBuffered(t *testing.T) {
	big := bytes.NewReader(bytes.Repeat([]byte("a"), maxRequestBodyBytes+1024))
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/api/v1/anything", big)
	rec := httptest.NewRecorder()

	var read int64
	var readErr error
	limitBody(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		read, readErr = io.Copy(io.Discard, r.Body)
	})).ServeHTTP(rec, req)

	if readErr == nil {
		t.Fatalf("read %d bytes with no error; the body was never capped", read)
	}
	if read > maxRequestBodyBytes {
		t.Errorf("buffered %d bytes, past the %d ceiling", read, maxRequestBodyBytes)
	}
}

// A body under the ceiling is untouched: the cap must not become a new failure mode.
func TestOrdinaryBodyPassesThroughUnchanged(t *testing.T) {
	body := strings.Repeat("x", 1024)
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/api/v1/anything", strings.NewReader(body))
	rec := httptest.NewRecorder()

	var got []byte
	limitBody(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		got, _ = io.ReadAll(r.Body)
	})).ServeHTTP(rec, req)

	if string(got) != body {
		t.Errorf("read %d bytes, want the %d that were sent", len(got), len(body))
	}
}

/* ── QA round 5: revocation must reach an OPEN socket ─────────────────────── */

// revocableAuthenticator answers a different subject (or an error) once flipped, the way the real
// one does after a token is revoked or a session is deleted.
type revocableAuthenticator struct {
	subject authz.Subject
	err     error
}

func (a *revocableAuthenticator) Authenticate(*http.Request) (authz.Subject, error) {
	return a.subject, a.err
}
func (a *revocableAuthenticator) Mode() string { return "fake" }

/*
 * Authorization used to be evaluated exactly once, at upgrade: revoking the credential left the
 * socket streaming topology, matrix and every live event until the browser closed it, with no audit
 * row for any of it. The connection now re-asks on its ping tick.
 */
func TestWSRevalidatorEndsTheConnectionWhenTheCredentialDies(t *testing.T) {
	subject := authz.Subject{Kind: authz.SubjectToken, ID: "tok-1", Roles: []string{"tester"}}
	auth := &revocableAuthenticator{subject: subject}
	policy := authz.NewPolicy(map[string][]authz.Permission{"tester": {authz.PermEventsRead}})
	s := newAuthzServer(t, auth, policy, Deps{Roles: fakeRoleResolver{roles: []string{"tester"}}})

	revalidate := s.wsRevalidator(httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/ws", nil), subjectCell(subject))
	if revalidate == nil {
		t.Fatal("no revalidator was built, so the upgrade's answer stands forever")
	}
	if err := revalidate(); err != nil {
		t.Fatalf("a live credential fails revalidation: %v", err)
	}

	// Revoked: the credential no longer authenticates.
	auth.err = errors.New("token revoked")
	if err := revalidate(); err == nil {
		t.Error("a revoked credential still passes revalidation")
	}

	// Re-issued to somebody else: same credential, different subject.
	auth.err = nil
	auth.subject = authz.Subject{Kind: authz.SubjectToken, ID: "tok-2"}
	if err := revalidate(); err == nil {
		t.Error("a credential that now resolves to another subject still passes revalidation")
	}
}

func TestWSRevalidatorEndsTheConnectionWhenThePermissionGoes(t *testing.T) {
	subject := authz.Subject{Kind: authz.SubjectUser, ID: "u1", Roles: []string{"tester"}}
	auth := &revocableAuthenticator{subject: subject}
	// A role that holds NEITHER events:read nor runs:read: the binding was deleted underneath a
	// connection that was admitted with it.
	policy := authz.NewPolicy(map[string][]authz.Permission{"tester": {authz.PermTopologyRead}})
	s := newAuthzServer(t, auth, policy, Deps{Roles: fakeRoleResolver{roles: []string{"tester"}}})

	err := s.wsRevalidator(httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/ws", nil), subjectCell(subject))()
	if err == nil {
		t.Error("a subject holding neither events:read nor runs:read keeps its socket")
	}
}

// subjectCell is the one-cell handoff handleWS builds: the topic gate and the revalidator share it,
// so both answer from the same, current subject.
func subjectCell(s authz.Subject) *atomic.Pointer[authz.Subject] { //nolint:gocritic // Subject is a value type by design
	cell := &atomic.Pointer[authz.Subject]{}
	cell.Store(&s)
	return cell
}

/*
 * Narrowing a role must reach a socket that is already open.
 *
 * The topic gate captured the subject BY VALUE at upgrade, so after an admin swapped a binding for a
 * narrower one the socket kept serving topology and matrix snapshots -- and admitted a FRESH
 * subscribe to them -- while GET /api/v1/topology answered 403 for the same user. Revocation only
 * took effect when the browser tab closed.
 */
func TestWSRevalidatorRepublishesTheSubjectSoTheTopicGateNarrows(t *testing.T) {
	subject := authz.Subject{Kind: authz.SubjectUser, ID: "u1", Roles: []string{"wide"}}
	auth := &revocableAuthenticator{subject: subject}
	policy := authz.NewPolicy(map[string][]authz.Permission{
		"wide":   {authz.PermEventsRead, authz.PermTopologyRead},
		"narrow": {authz.PermEventsRead},
	})
	roles := &mutableRoleResolver{roles: []string{"wide"}}
	s := newAuthzServer(t, auth, policy, Deps{Roles: roles})

	cell := subjectCell(subject)
	authorize := s.wsTopicAuthorizer(cell)
	revalidate := s.wsRevalidator(httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/ws", nil), cell)

	if err := authorize(ws.TopicTopology); err != nil {
		t.Fatalf("setup: the wide role was refused the topology topic: %v", err)
	}

	// The admin swaps the binding for a narrower role. The CONNECTION survives -- the subject still
	// holds events:read -- but the topic it no longer has must stop being admitted.
	roles.set([]string{"narrow"})
	if err := revalidate(); err != nil {
		t.Errorf("the socket was closed over a change that touched one topic: %v", err)
	}
	if err := authorize(ws.TopicTopology); err == nil {
		t.Error("the topic gate still admits topology after the role was narrowed")
	}
	if err := authorize(ws.TopicLive); err != nil {
		t.Errorf("a topic the subject still holds was refused: %v", err)
	}
}

// mutableRoleResolver is fakeRoleResolver whose answer can change mid-connection, which is the whole
// point of the revalidator.
type mutableRoleResolver struct {
	mu    sync.Mutex
	roles []string
}

func (r *mutableRoleResolver) set(roles []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.roles = roles
}

func (r *mutableRoleResolver) RolesFor(_ context.Context, _ authz.Subject) ([]string, error) { //nolint:gocritic // Subject is a value type by design
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.roles...), nil
}

/*
 * Revocation follows the PERMISSIONS, not the role names.
 *
 * Comparing name lists missed the ordinary way an admin narrows access -- editing the role's
 * permission set in place, which leaves the names identical -- so the socket kept streaming topics
 * its subject had just lost. It also read a binding rewritten to the same roles in a different order
 * as a change, closing a healthy connection for nothing.
 */
func TestWSRevalidatorFollowsPermissionsNotRoleNames(t *testing.T) {
	subject := authz.Subject{Kind: authz.SubjectUser, ID: "u1", Roles: []string{"editable"}}
	auth := &revocableAuthenticator{subject: subject}
	policy := authz.NewPolicy(map[string][]authz.Permission{
		"editable": {authz.PermEventsRead, authz.PermTopologyRead},
	})
	s := newAuthzServer(t, auth, policy, Deps{Roles: fakeRoleResolver{roles: []string{"editable"}}})

	cell := subjectCell(subject)
	revalidate := s.wsRevalidator(httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/ws", nil), cell)
	if err := revalidate(); err != nil {
		t.Fatalf("setup: a live subject fails revalidation: %v", err)
	}

	// The ROLE is edited in place: same name, fewer permissions. The gate must follow the
	// permissions even though the name list did not move.
	policy.Reload(map[string][]authz.Permission{"editable": {authz.PermEventsRead}})
	if err := revalidate(); err != nil {
		t.Errorf("the socket was closed over a permission change it can survive: %v", err)
	}
	if err := s.wsTopicAuthorizer(cell)(ws.TopicTopology); err == nil {
		t.Error("the topic gate still admits topology after the role lost topology:read")
	}
}

// And a role list that merely CHANGED ORDER is not a change: closing there costs a reconnect for
// nothing.
func TestWSRevalidatorIgnoresARoleReorder(t *testing.T) {
	subject := authz.Subject{Kind: authz.SubjectUser, ID: "u1", Roles: []string{"a", "b"}}
	auth := &revocableAuthenticator{subject: subject}
	policy := authz.NewPolicy(map[string][]authz.Permission{
		"a": {authz.PermEventsRead},
		"b": {authz.PermTopologyRead},
	})
	roles := &mutableRoleResolver{roles: []string{"a", "b"}}
	s := newAuthzServer(t, auth, policy, Deps{Roles: roles})

	cell := subjectCell(subject)
	revalidate := s.wsRevalidator(httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/ws", nil), cell)
	if err := revalidate(); err != nil {
		t.Fatalf("setup: %v", err)
	}

	roles.set([]string{"b", "a"})
	if err := revalidate(); err != nil {
		t.Errorf("a role REORDER closed the socket: %v", err)
	}
}
