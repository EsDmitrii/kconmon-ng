package httpapi

import (
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/EsDmitrii/kconmon-ng/internal/console/store"
)

// K8sEventService is the READ half of the captured Kubernetes event table,
// narrowed the same way EventLister narrows store.EventStore: the writer is
// internal/console/kubectx (M6 Task 2) and the HTTP layer must not be able to
// forge a cluster event.
type K8sEventService interface {
	store.K8sEventReader
}

var _ K8sEventService = (*store.DB)(nil)

// k8sEventsUnavailableDetail names BOTH knobs, because they answer two
// different questions and an operator who is told only one of them will fix
// the wrong thing.
//
// This 503 is about the FIRST one only: s.k8sEvents is nil exactly when there
// is no database, and no database means there is nowhere for the reader to
// have written anything. The second knob never produces a 503 -- with a
// database and the reader off, this route answers 200 with an empty page,
// which is the honest report of "nothing was captured", not of "this endpoint
// is unavailable". Saying so here is what stops an operator from concluding
// their empty timeline is a broken API.
const k8sEventsUnavailableDetail = "captured Kubernetes events live in the database and have no in-memory " +
	"fallback: set console.database.mode (Helm: console.database.mode) to enable GET /api/v1/k8s-events, and " +
	"console.kubernetesContext.enabled to capture events into it -- without the capture the endpoint answers " +
	"an empty page rather than this error"

// k8sEventKinds and k8sEventTypes are the closed vocabularies store's own
// validation enforces on the write side (store/k8sevents.go). A ?kind= or
// ?type= outside them can never match a row, so it is rejected as a likely
// typo rather than silently returning an empty page -- handleEvents' ?type=
// precedent.
var (
	k8sEventKinds = map[string]bool{"Node": true, "Pod": true}
	k8sEventTypes = map[string]bool{"Normal": true, "Warning": true}
)

// k8sEventResponse is one captured cluster event on the wire.
//
// ID is a STRING even though the column is a bigint, for store.PinnedRef's
// reason: an incident pins findings from six different tables into ONE
// heterogeneous list, and one id spelling across it is worth more than a
// per-source type every reader would have to switch on. The timeline pins
// these by the value this field carries.
type k8sEventResponse struct {
	ID              string    `json:"id"`
	UID             string    `json:"uid"`
	ResourceVersion string    `json:"resourceVersion"`
	EventTime       time.Time `json:"eventTime"`
	Kind            string    `json:"kind"`
	Name            string    `json:"name"`
	// Namespace is "" for the cluster-scoped Node events.
	Namespace string `json:"namespace"`
	Reason    string `json:"reason"`
	Type      string `json:"type"`
	Message   string `json:"message"`
	Count     int32  `json:"count"`
}

func k8sEventResponseFrom(e *store.K8sEvent) k8sEventResponse {
	return k8sEventResponse{
		ID: strconv.FormatInt(e.ID, 10), UID: e.UID, ResourceVersion: e.ResourceVersion,
		EventTime: e.EventTime, Kind: e.Kind, Name: e.Name, Namespace: e.Namespace,
		Reason: e.Reason, Type: e.Type, Message: e.Message, Count: e.Count,
	}
}

// k8sEventsListResponse is GET /api/v1/k8s-events's body -- the same
// keyset-cursor shape as eventsResponse, and the same (event_time, id) bigint
// cursor codec behind it.
type k8sEventsListResponse struct {
	Events     []k8sEventResponse `json:"events"`
	NextCursor string             `json:"nextCursor"`
}

// handleK8sEvents serves one page of captured Kubernetes events, newest first.
//
// It rides events:read rather than a permission of its own (M6 Decision 8):
// these ARE events, and a separate permission would gate nothing an operator
// holding events:read could not already infer from the topology and run
// streams they can read.
//
// Filters are all exact matches -- ?name= (a node or pod name), ?kind=
// (Node|Pod), ?type= (Normal|Warning) -- plus the usual from/to window over
// event_time. Unlike handleEvents this does NOT reject an inverted window: the
// caller is the Investigate timeline, driven by a chart's visible range, and a
// degenerate range there is a range with nothing in it, not a client bug worth
// a 400 (handleAnnotationsList's reasoning).
func (s *Server) handleK8sEvents(w http.ResponseWriter, r *http.Request) {
	if s.k8sEvents == nil {
		writeProblem(w, http.StatusServiceUnavailable, "kubernetes events not available",
			k8sEventsUnavailableDetail)
		return
	}
	q := r.URL.Query()

	kind := q.Get("kind")
	if kind != "" && !k8sEventKinds[kind] {
		writeProblem(w, http.StatusBadRequest, "invalid kind", "kind must be one of Node, Pod")
		return
	}
	eventType := q.Get("type")
	if eventType != "" && !k8sEventTypes[eventType] {
		writeProblem(w, http.StatusBadRequest, "invalid type", "type must be one of Normal, Warning")
		return
	}

	from, ok := parseEventsTime(w, q.Get("from"), "from")
	if !ok {
		return
	}
	to, ok := parseEventsTime(w, q.Get("to"), "to")
	if !ok {
		return
	}

	cursor := q.Get("cursor")
	if cursor != "" {
		// Validated up front so a broken cursor is the client's 400, never a
		// 502 that reads as an outage and never a silent restart from the top.
		if _, _, _, err := store.DecodeCursor(cursor); err != nil {
			writeProblem(w, http.StatusBadRequest, "invalid cursor", "cursor is malformed or does not match this server")
			return
		}
	}

	page, err := s.k8sEvents.ListK8sEvents(r.Context(), store.K8sEventFilter{
		Name:   q.Get("name"),
		Kind:   kind,
		Type:   eventType,
		From:   from,
		To:     to,
		Cursor: cursor,
		Limit:  clampPageLimit(parsePageLimit(q.Get("limit"))),
	})
	if err != nil {
		// Logged, never surfaced: the driver error can carry a DSN.
		slog.Error("list kubernetes events failed", "error", err)
		writeProblem(w, http.StatusBadGateway, "kubernetes events unavailable",
			"failed to query captured kubernetes events")
		return
	}

	out := make([]k8sEventResponse, 0, len(page.Events))
	for i := range page.Events {
		out = append(out, k8sEventResponseFrom(&page.Events[i]))
	}
	writeJSON(w, k8sEventsListResponse{Events: out, NextCursor: page.NextCursor})
}
