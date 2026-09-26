package controller

import "net/http"

// notLeaderMsg is what every leader-only endpoint answers on a standby. The CLI's isStandbyAnswer
// matches this text to move on to the next replica, so it must not change.
const notLeaderMsg = "not the leader"

// leaderGate is the leadership check of the leader-only endpoints.
type leaderGate struct {
	enabled  bool
	isLeader func() bool
}

// lost reports whether leader election is on and this replica is not, or no longer, the leader. A
// nil gate (one not injected yet) never refuses.
func (g *leaderGate) lost() bool {
	return g != nil && g.enabled && (g.isLeader == nil || !g.isLeader())
}

// refuse answers 503 notLeaderMsg when the gate is lost and reports whether it did.
func (g *leaderGate) refuse(w http.ResponseWriter) bool {
	if !g.lost() {
		return false
	}
	http.Error(w, notLeaderMsg, http.StatusServiceUnavailable)
	return true
}
