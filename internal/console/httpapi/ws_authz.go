package httpapi

import (
	"errors"

	"github.com/EsDmitrii/kconmon-ng/internal/console/authz"
	"github.com/EsDmitrii/kconmon-ng/internal/console/ws"
)

// wsTopicAuthorizer builds the PER-CONNECTION topic gate for one /ws upgrade; until the socket had
// exactly one authorization decision — routeTable's "GET /ws" row.
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
		// Delivered verbatim to the client as the error frame's detail (ws.TopicAuthorizer's contract).
		return errors.New("missing permission: " + string(authz.PermEventsRead) +
			"; this connection was admitted on " + string(authz.PermRunsRead) +
			", which covers run:{id} topics only")
	}
}
