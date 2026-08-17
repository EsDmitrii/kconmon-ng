package httpapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/EsDmitrii/kconmon-ng/internal/console/authz"
	"github.com/EsDmitrii/kconmon-ng/internal/console/ws"
)

/*
The socket carries the same bytes the REST routes do, so it has to ask the same questions.

It used to ask one: whoever held events:read got a nil authorizer, i.e. the whole static topic
allowlist -- including the topology and matrix snapshots, which push/topology.go and push/matrix.go
marshal from the very calls GET /api/v1/topology and GET /api/v1/matrix make. A custom role holding
events:read but not topology:read read the fleet's inventory over the socket after the REST route had
answered 403, and the read left no audit row. Symmetrically, a subject holding topology:read but not
events:read was refused a topic it was entitled to.
*/

// wsTopicPermission maps one subscribable topic to the permission it requires, mirroring routeTable's
// split for the REST routes that serve the same bytes. A run:{id} topic answers "": it is covered by
// the /ws upgrade gate itself (events:read OR runs:read) and by nothing narrower.
//
// The matrix arm matches the PREFIX rather than today's three MatrixTopic values, so adding a
// protocol to the allowlist cannot silently fall through to events:read. The hub checks that static
// allowlist before it calls this, so the default arm only ever sees the live feed.
func wsTopicPermission(topic string) authz.Permission {
	switch {
	case ws.IsRunTopic(topic):
		return ""
	case topic == ws.TopicTopology:
		return authz.PermTopologyRead
	case strings.HasPrefix(topic, "matrix:"):
		return authz.PermMatrixRead
	default:
		return authz.PermEventsRead
	}
}

// wsTopicAuthorizer builds the PER-CONNECTION topic gate for one /ws upgrade: the route table's
// anyOf row admits the socket, this decides every topic carried on it.
func (s *Server) wsTopicAuthorizer(current *atomic.Pointer[authz.Subject]) ws.TopicAuthorizer {
	return func(topic string) error {
		perm := wsTopicPermission(topic)
		/* The CURRENT subject, not the upgrade-time snapshot.
		   This closure used to capture the subject by value, so a narrowed role binding left the gate
		   answering from the roles the user held when the tab was opened: a fresh `subscribe topology`
		   on that same socket was still admitted after GET /api/v1/topology had started answering 403.
		   The revalidator publishes here on every ping tick. */
		subject := current.Load()
		if perm == "" || (subject != nil && s.policy.Can(*subject, perm)) {
			return nil
		}
		// Delivered verbatim to the client as the error frame's detail (ws.TopicAuthorizer's contract).
		return errors.New("missing permission: " + string(perm))
	}
}

/*
 * wsRevalidator re-answers "may this connection still exist?" on every ping tick.
 *
 * The socket carries what the REST routes carry, and those are re-authorized on every request. The
 * socket was authorized once, at upgrade, and then trusted for its whole life: revoking the token,
 * deleting the role binding or ending the session changed nothing until the browser tab closed.
 *
 * It re-runs the SAME authenticator against the upgrade request — the credential lives in its
 * headers and cookies, and both are immutable once the request is in flight — and then re-checks the
 * gate the route table applies to /ws. A subject that no longer authenticates, or no longer holds
 * either permission, ends the connection.
 *
 * Anonymous mode has no credential to revoke, so the check there is only the permission one; that is
 * exactly right, since the anonymous role can be changed underneath a running console.
 */
func (s *Server) wsRevalidator(r *http.Request, current *atomic.Pointer[authz.Subject]) func() error {
	if s.authenticator == nil {
		return nil
	}
	at := *current.Load()
	// The request is kept for its credential only; its context ends with the upgrade, so a fresh,
	// bounded one is used for the store lookups a re-authentication makes.
	creds := r.Clone(context.Background())

	return func() error {
		ctx, cancel := context.WithTimeout(context.Background(), wsRevalidateTimeout)
		defer cancel()

		subject, err := s.authenticator.Authenticate(creds.WithContext(ctx))
		if err != nil {
			return fmt.Errorf("credential no longer authenticates: %w", err)
		}
		if subject.Kind != at.Kind || subject.ID != at.ID {
			// A different subject on the same credential is not this connection's subject.
			return errors.New("credential now resolves to a different subject")
		}
		/* The BINDINGS are re-read too, the same way the request middleware reads them: deleting a
		   role binding is the other half of revocation, and Authenticate knows nothing about it. */
		subject = s.resolveRoles(ctx, subject)
		if !s.policy.Can(subject, authz.PermEventsRead) && !s.policy.Can(subject, authz.PermRunsRead) {
			return errors.New("subject no longer holds events:read or runs:read")
		}
		/* Publish the fresh subject and let the connection re-gate its own topics.
		   The topic gate reads this pointer, so republishing is what makes a narrowed permission
		   reach an already-open socket; ws.Hub.regate then drops exactly the subscriptions that are
		   no longer permitted and sends the page an error frame naming each. Ending the whole
		   connection was the first fix for this and it was too blunt -- it cost every OTHER topic on
		   the socket a reconnect and a resubscribe for a change that touched one of them. The socket
		   itself still ends when the COARSE gate above stops holding, which is the answer to "may
		   this connection exist at all"; this is the answer to "may it still have THIS topic". */
		current.Store(&subject)
		return nil
	}
}

// wsRevalidateTimeout bounds ONE re-authentication (a session or token lookup in the store).
const wsRevalidateTimeout = 3 * time.Second
