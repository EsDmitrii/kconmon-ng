package httpapi

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/EsDmitrii/kconmon-ng/internal/checker"
	"github.com/EsDmitrii/kconmon-ng/internal/console/store"
)

/*
 * refuseUnrunnableDefinition rejects a check definition NO AGENT COULD EVER RUN.
 *
 * The agent parses every assignment entry with checker.ParseExternalSpec and drops the ones it
 * cannot: checkType=http against a target of kind host (the parser wants an http(s):// URL),
 * checkType=dns with no params.query. That drop was agent-local — a WARN in one pod's log, repeated
 * on every assignment push, forever — while the Console went on listing the definition as enabled
 * and the operator waited for results that could not arrive from any node.
 *
 * Refusing it here is the honest place: the parser is a pure function of fields the caller has just
 * typed, so the answer is available before the row exists. The reconciler keeps its own copy of this
 * check as a backstop for rows written before this guard (see checks.skipUnrunnable).
 *
 * It is deliberately NARROW. Only a definition that would become a continuous external check is
 * parsed: a node destination is the agents' own peer mesh, and a check type the external checker
 * does not serve (mtr, udp) is skipped by the reconciler for its own reason. Anything this function
 * cannot resolve — an unreadable target row — is left to the store, which produces the specific
 * error the caller needs.
 */
func (s *Server) refuseUnrunnableDefinition(w http.ResponseWriter, r *http.Request, in *store.DefinitionInput) bool {
	if !externalParseableCheckTypes[in.CheckType] || in.DestinationKind == "node" {
		return false
	}

	name, kind, address, ok := s.definitionDestination(r.Context(), in)
	if !ok {
		return false
	}

	target, port := splitExternalAddress(kind, address)
	_, err := checker.ParseExternalSpec(&checker.ExternalSpecInput{
		Name:    name,
		Address: target,
		Port:    port,
		// The interval and timeout the reconciler stamps; the parser rejects a non-positive one and
		// this route never sets them, so passing the same constants keeps the two answers identical.
		CheckType:  in.CheckType,
		Interval:   continuousProbeInterval,
		Timeout:    continuousProbeTimeout,
		ParamsJSON: in.Params,
	})
	if err == nil {
		return false
	}
	writeProblem(w, http.StatusUnprocessableEntity, "check definition cannot run",
		"definition: no agent could run this check as written: "+err.Error())
	return true
}

/*
 * continuousProbeInterval and continuousProbeTimeout mirror the reconciler's defaults
 * (internal/console/checks: defaultContinuousInterval / defaultContinuousTimeout).
 *
 * They exist here so the guard parses the SAME spec the reconciler will build. Only their sign
 * matters to the parser, but a value that drifted from the reconciler's would make this route accept
 * something the push then skips, which is the split this guard closes.
 */
const (
	continuousProbeInterval = 30 * time.Second
	continuousProbeTimeout  = 5 * time.Second
)

// externalParseableCheckTypes is the set the external checker serves; it mirrors
// checks.externalCheckTypes and controller's validExternalCheckTypes.
var externalParseableCheckTypes = map[string]bool{"tcp": true, "icmp": true, "dns": true, "http": true}

// definitionDestination resolves the name/kind/address the reconciler would put on the wire, and
// reports whether it could. A destination it cannot read is NOT an error here: the store's own
// answer to the write is the specific one.
func (s *Server) definitionDestination(ctx context.Context, in *store.DefinitionInput) (name, kind, address string, ok bool) {
	if in.DestinationKind != "target" {
		// "adhoc" — the definition's own name travels as the target name, exactly as the reconciler
		// does it, so a message naming the target names something the operator recognises.
		return in.Name, adhocDestinationKind(in.DestinationAddress), in.DestinationAddress, true
	}
	if s.targets == nil || in.DestinationTargetID == "" {
		return "", "", "", false
	}
	target, err := s.targets.GetTarget(ctx, in.DestinationTargetID)
	if err != nil {
		return "", "", "", false
	}
	return target.Name, target.Kind, target.Address, true
}

// adhocDestinationKind mirrors checks.adhocTargetKind: an address that parses as a URL is a "url"
// destination, everything else is a "host".
func adhocDestinationKind(address string) string {
	if strings.Contains(address, "://") {
		return "url"
	}
	return "host"
}

// splitExternalAddress mirrors checks.externalTarget's split: a host destination may carry its port
// in the address, and the parser wants them apart.
func splitExternalAddress(kind, address string) (host string, port uint32) {
	if kind != "host" {
		return address, 0
	}
	h, p, found := strings.Cut(address, ":")
	if !found || h == "" {
		return address, 0
	}
	n, err := strconv.Atoi(p)
	if err != nil || n < 1 || n > 65535 {
		return address, 0
	}
	return h, uint32(n) //nolint:gosec // bounded to 1..65535 immediately above
}
