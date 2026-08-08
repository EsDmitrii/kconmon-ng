package httpapi

import (
	"errors"

	"github.com/EsDmitrii/kconmon-ng/internal/console/authz"
	"github.com/EsDmitrii/kconmon-ng/internal/console/ws"
)

// wsTopicAuthorizer builds the PER-CONNECTION topic gate for one /ws upgrade,
// closing M3 follow-up #10 ("ws events:read for run-watching custom roles").
//
// The problem it solves. Until M7 the socket had exactly one authorization
// decision — routeTable's "GET /ws" row, events:read — and that single
// decision covered every multiplexed topic, because ws.Hub had no way to tell
// its connections apart (SECURITY.md §10.2: "ws.Hub never receives an
// authz.Subject ... so there is no layer that could gate one topic differently
// from another"). The consequence was a real usability hole with no safe
// workaround: a custom role granted runs:read so it could START a diagnostics
// run could not open the socket to WATCH the run it had just started, and had
// to poll GET /api/v1/runs/{id} instead. The obvious fix — lowering the route
// to runs:read — was rejected in M4 for an equally real reason: it would have
// handed every run watcher the fleet-wide "live" event stream, which is
// exactly what events:read gates on GET /api/v1/events. Neither permission
// alone was the right answer because the question was never per-connection in
// the first place.
//
// The shape of the fix. Two decisions instead of one, at the two layers that
// can actually make them:
//
//   - The UPGRADE is admitted for events:read OR runs:read (routeTable's
//     anyOf). That is the only change on the route table, and it is what lets
//     a runs:read-only subject reach the hub at all.
//   - Each SUBSCRIBE is then gated by the closure this function returns, which
//     the hub captures on that one connection (ws.Hub.ServeWSAuthorized). A
//     subject holding events:read gets nil — no per-frame work and behaviour
//     byte-identical to pre-M7. A subject that got in on runs:read alone gets
//     run:{id} topics and nothing else.
//
// So the widening is exactly one thing and no more: run watching. live,
// topology and matrix:*:pod still need events:read, on the socket just as on
// the REST routes, which is the property the M4 decision was protecting.
//
// Why the run: topics are safe for runs:read. A run:{id} topic carries that
// run's own progress frames and its terminal summary — the same content
// GET /api/v1/runs/{id} already returns to a runs:read caller, delivered
// promptly instead of on a poll. Nothing else is published on it.
//
// Not a per-RUN ownership check. runs:read is a fleet-wide permission (a
// subject holding it may already GET any run by id), so this authorizes the
// CLASS of topic, not one subject's own runs. Narrowing to "runs you started"
// would be a different feature needing a per-run owner column, and it would
// not be a tightening of anything that exists today.
func (s *Server) wsTopicAuthorizer(subject authz.Subject) ws.TopicAuthorizer { //nolint:gocritic // Subject is a value type by design (authz package doc)
	if s.policy.Can(subject, authz.PermEventsRead) {
		// The pre-M7 path, deliberately identical to it: no closure, no
		// per-subscribe call, no behaviour change for viewer/operator/admin
		// or any custom role that holds events:read.
		return nil
	}
	return func(topic string) error {
		if ws.IsRunTopic(topic) {
			return nil
		}
		// Delivered verbatim to the client as the error frame's detail
		// (ws.TopicAuthorizer's contract), so it names the permission to ask
		// an admin for rather than leaving a browser to guess. It reveals
		// nothing: the topic string came from the client, and which
		// permissions exist is public (GET /api/v1/rbac/permissions).
		return errors.New("missing permission: " + string(authz.PermEventsRead) +
			"; this connection was admitted on " + string(authz.PermRunsRead) +
			", which covers run:{id} topics only")
	}
}
