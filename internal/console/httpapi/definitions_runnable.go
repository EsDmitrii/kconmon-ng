package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/EsDmitrii/kconmon-ng/internal/checker"
	"github.com/EsDmitrii/kconmon-ng/internal/console/checks"
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
 * parsed: a node destination is the agents' own peer mesh, and mtr runs toward an external
 * destination in one-off runs, so only its continuous schedule is refused. udp toward one is refused
 * outright: no path runs it (errUDPNodesOnly). Anything this function
 * cannot resolve — an unreadable target row — is left to the store, which produces the specific
 * error the caller needs.
 */
func (s *Server) refuseUnrunnableDefinition(w http.ResponseWriter, r *http.Request, in *store.DefinitionInput) bool {
	err := s.unrunnableDefinition(r.Context(), in)
	if err == nil {
		return false
	}
	writeProblem(w, http.StatusUnprocessableEntity, "check definition cannot run", unrunnableDefinitionDetail(err))
	return true
}

// unrunnableDefinition is refuseUnrunnableDefinition without a ResponseWriter: the parser's refusal,
// or nil when the definition runs, is not an external check, or its destination cannot be read.
func (s *Server) unrunnableDefinition(ctx context.Context, in *store.DefinitionInput) error {
	if !runsAsExternalCheck(in.CheckType, in.DestinationKind) {
		return nil
	}
	if in.CheckType == "udp" {
		return errUDPNodesOnly
	}
	name, kind, address, ok := s.definitionDestination(ctx, in)
	if !ok {
		return nil
	}
	return externalSpecError(in.CheckType, in.Params, name, kind, address)
}

func unrunnableDefinitionDetail(err error) string {
	return "definition: no agent could run this check as written: " + err.Error()
}

// runsAsExternalCheck reports whether a definition toward an external destination is judged here: a
// continuous external check the agent parses, or udp, which nothing runs toward one. mtr runs toward
// one in one-off runs, so only its continuous schedule is refused (scheduleCannotRun).
func runsAsExternalCheck(checkType, destinationKind string) bool {
	return (externalParseableCheckTypes[checkType] || checkType == "udp") && destinationKind != "node"
}

// errUDPNodesOnly refuses udp toward a target or ad-hoc destination: the reconciler skips it as a
// continuous check and the agent refuses it in a run, so no path could ever produce a result.
var errUDPNodesOnly = errors.New("check type udp probes kconmon nodes only: " +
	"the udp probe needs the far end to echo its sequence number back, which only a kconmon agent does")

// externalSpecError runs the agent's parser on the spec the reconciler would build for a definition
// of checkType against the named destination.
func externalSpecError(checkType string, params json.RawMessage, name, kind, address string) error {
	if checkType == "udp" {
		return errUDPNodesOnly
	}
	target, port := splitExternalAddress(kind, address)
	_, err := checker.ParseExternalSpec(&checker.ExternalSpecInput{
		Name:    name,
		Address: target,
		Port:    port,
		// The interval and timeout the reconciler stamps; the parser rejects a non-positive one and
		// this route never sets them, so passing the same constants keeps the two answers identical.
		CheckType:  checkType,
		Interval:   continuousProbeInterval,
		Timeout:    continuousProbeTimeout,
		ParamsJSON: params,
	})
	return err
}

/*
definitionsBrokenByTargetEdit is the same guard seen from the target: it lists the definitions
pointing at target id that run against the target as stored and would stop running against next,
and returns the parser's refusal for the first of them by name; exact is false when the walk
stopped before the last page.

A PUT that turns a url target into a host one otherwise left every http definition on it listed as
enabled while the reconciler skipped it as unrunnable, and re-saving that definition unchanged was
then refused. Definitions that could not run before the edit are not the edit's doing and do not
block it, and neither do the ones named in replaced (an import that rewrites them judges them as it
writes them). A target or a definition list that cannot be read fails open, as the definition-side
guard does: the store's own answer to the write is the specific one.
*/
func (s *Server) definitionsBrokenByTargetEdit(
	ctx context.Context, id string, next *store.TargetInput, replaced map[string]bool,
) (names []string, exact bool, reason error) {
	if s.definitions == nil {
		return nil, true, nil
	}
	cur, getErr := s.targets.GetTarget(ctx, id)
	if getErr != nil {
		return nil, true, nil //nolint:nilerr // fails open: UpdateTarget gives the caller the specific answer
	}
	if cur.Name == next.Name && cur.Kind == next.Kind && cur.Address == next.Address {
		return nil, true, nil
	}
	reasons := map[string]error{}
	filter := store.DefinitionFilter{TargetID: id, Limit: inUseDefinitionsPageSize}
	for range inUseDefinitionsMaxPages {
		page, listErr := s.definitions.ListDefinitions(ctx, filter)
		if listErr != nil {
			slog.Warn("httpapi: list definitions for a target edit failed, allowing the write", //nolint:gosec // G706: structured slog fields, not string-built log injection
				"target", id, "error", listErr)
			break
		}
		for i := range page.Definitions {
			d := &page.Definitions[i]
			if replaced[d.Name] || !runsAsExternalCheck(d.CheckType, d.DestinationKind) {
				continue
			}
			nextErr := externalSpecError(d.CheckType, d.Params, next.Name, next.Kind, next.Address)
			if nextErr == nil || externalSpecError(d.CheckType, d.Params, cur.Name, cur.Kind, cur.Address) != nil {
				continue
			}
			reasons[d.Name] = nextErr
			names = append(names, d.Name)
		}
		if page.NextCursor == "" {
			exact = true
			break
		}
		filter.Cursor = page.NextCursor
	}
	if len(names) == 0 {
		return nil, true, nil
	}
	return names, exact, reasons[slices.Min(names)]
}

// targetEditBreaksDetail words definitionsBrokenByTargetEdit's answer for the 422 and the import's
// per-item error.
func targetEditBreaksDetail(next *store.TargetInput, names []string, exact bool, reason error) string {
	listed, total := definitionNameList(names, exact)
	return "target: check " + pluralDefinitions(total) + " " + listed +
		" could no longer run against " + next.Kind + " " + strconv.Quote(next.Address) + ": " + reason.Error() +
		"; change or re-point " + pronounFor(total) + " first"
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

/*
scheduleCannotRun reports why a schedule of kind could never run a definition of checkType toward
destinationKind, or nil. A continuous schedule becomes an external check on the agents, which serve
tcp, icmp, dns and http; a once or interval schedule fires runs, which the agent accepts toward an
external destination for tcp, icmp and mtr only (checks.RefuseExternalRun). Accepting either mismatch
would store a schedule the reconciler skips or whose every fire fails every pair.

A node destination is left alone: it is the agents' peer mesh, every type runs toward it.
*/
func scheduleCannotRun(kind, checkType, destinationKind string) error {
	if destinationKind == "node" {
		return nil
	}
	if checkType == "udp" {
		return errUDPNodesOnly
	}
	switch kind {
	case "continuous":
		if externalParseableCheckTypes[checkType] {
			return nil
		}
		return fmt.Errorf("check type %s cannot run continuously toward destination kind %q: "+
			"the agents run only tcp, icmp, dns and http checks continuously toward one", checkType, destinationKind)
	case "once", "interval":
		return checks.RefuseExternalRun(checkType, destinationKind)
	}
	return nil
}

// refuseUnrunnableSchedule answers 422 for a schedule whose kind cannot run its definition's check.
// A definition it cannot read is left to the store, whose answer to the write is the specific one.
func (s *Server) refuseUnrunnableSchedule(w http.ResponseWriter, r *http.Request, in *store.ScheduleInput) bool {
	if s.definitions == nil {
		return false
	}
	def, err := s.definitions.GetDefinition(r.Context(), in.DefinitionID)
	if err != nil {
		return false
	}
	if cerr := scheduleCannotRun(in.Kind, def.CheckType, def.DestinationKind); cerr != nil {
		writeProblem(w, http.StatusUnprocessableEntity, "invalid schedule", "schedule: "+cerr.Error())
		return true
	}
	return false
}

/*
refuseDefinitionEditBreakingSchedules is the same guard seen from the definition: a PUT that changes
the check type or the destination kind so that one of the definition's own schedules could no longer
run it is refused, naming that schedule's kind. A schedule that could not run the definition before
the edit is not the edit's doing and does not block it. Reads that fail leave the write to the store.
*/
func (s *Server) refuseDefinitionEditBreakingSchedules(w http.ResponseWriter, r *http.Request, id string, next *store.DefinitionInput) bool {
	kind, reason := s.scheduleBrokenByDefinitionEdit(r.Context(), id, next)
	if reason == nil {
		return false
	}
	writeProblem(w, http.StatusUnprocessableEntity, "check definition cannot run", definitionEditBreaksScheduleDetail(kind, reason))
	return true
}

// definitionEditBreaksScheduleDetail words scheduleBrokenByDefinitionEdit's answer for the 422 and
// an import's per-item error.
func definitionEditBreaksScheduleDetail(kind string, reason error) string {
	return "definition: its " + kind + " schedule could not run the edited check: " + reason.Error() +
		"; change or delete that schedule first"
}

// scheduleBrokenByDefinitionEdit returns the kind of the first schedule of definition id that runs it
// as stored and could not run next, with the reason, or a nil reason.
func (s *Server) scheduleBrokenByDefinitionEdit(ctx context.Context, id string, next *store.DefinitionInput) (kind string, reason error) {
	if s.schedules == nil || s.definitions == nil {
		return "", nil
	}
	cur, err := s.definitions.GetDefinition(ctx, id)
	if err != nil || (cur.CheckType == next.CheckType && cur.DestinationKind == next.DestinationKind) {
		return "", nil //nolint:nilerr // fails open: UpdateDefinition gives the caller the specific answer
	}
	filter := store.ScheduleFilter{DefinitionID: id, Limit: inUseDefinitionsPageSize}
	for range inUseDefinitionsMaxPages {
		page, listErr := s.schedules.ListSchedules(ctx, filter)
		if listErr != nil {
			slog.Warn("httpapi: list schedules for a definition edit failed, allowing the write", //nolint:gosec // G706: structured slog fields, not string-built log injection
				"definition", id, "error", listErr)
			return "", nil
		}
		for i := range page.Schedules {
			k := page.Schedules[i].Kind
			cerr := scheduleCannotRun(k, next.CheckType, next.DestinationKind)
			if cerr != nil && scheduleCannotRun(k, cur.CheckType, cur.DestinationKind) == nil {
				return k, cerr
			}
		}
		if page.NextCursor == "" {
			return "", nil
		}
		filter.Cursor = page.NextCursor
	}
	return "", nil
}
