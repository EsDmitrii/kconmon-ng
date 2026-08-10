package httpapi

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/EsDmitrii/kconmon-ng/internal/console/authz"
	"github.com/EsDmitrii/kconmon-ng/internal/console/store"
)

// AnnotationService is the subset of *store.DB httpapi needs for /api/v1/annotations.
type AnnotationService interface {
	store.AnnotationReader
	store.AnnotationStore
}

var _ AnnotationService = (*store.DB)(nil)

// annotationsUnavailableDetail is served whenever s.annotations is nil, in
// targetsUnavailableDetail's shape.
const annotationsUnavailableDetail = "annotations are persisted operator notes with no in-memory fallback: " +
	"set console.database.mode in the console config (Helm: console.database.mode) to enable /api/v1/annotations"

// annotationsUnavailable answers 503 and reports true when no
// AnnotationService is wired.
func (s *Server) annotationsUnavailable(w http.ResponseWriter) bool {
	if s.annotations == nil {
		writeProblem(w, http.StatusServiceUnavailable, "annotations not available", annotationsUnavailableDetail)
		return true
	}
	return false
}

// annotationResponse is one mark on the wire. EndAt is omitted for an
// INSTANT mark -- which means "a point in time", not "still open".
type annotationResponse struct {
	ID      string     `json:"id"`
	StartAt time.Time  `json:"startAt"`
	EndAt   *time.Time `json:"endAt,omitempty"`
	// Scope is "" for a global annotation; any other value names a node, a
	// pair or a target and is matched exactly. It is a filter key, never a
	// Prometheus label value.
	Scope     string    `json:"scope"`
	Text      string    `json:"text"`
	CreatedBy string    `json:"createdBy"`
	CreatedAt time.Time `json:"createdAt"`
}

func annotationResponseFrom(a *store.Annotation) annotationResponse {
	return annotationResponse{
		ID: a.ID, StartAt: a.StartAt, EndAt: a.EndAt, Scope: a.Scope,
		Text: a.Text, CreatedBy: a.CreatedBy, CreatedAt: a.CreatedAt,
	}
}

// annotationsListResponse is GET /api/v1/annotations's body -- the same
// keyset-cursor shape as targetsListResponse.
type annotationsListResponse struct {
	Annotations []annotationResponse `json:"annotations"`
	NextCursor  string               `json:"nextCursor"`
}

// annotationRequest is POST /api/v1/annotations's body. There is deliberately
// no createdBy field: attribution is the SERVER's view of who wrote the note
// (annotationAuthor below), never something a client can state.
type annotationRequest struct {
	StartAt time.Time  `json:"startAt"`
	EndAt   *time.Time `json:"endAt"`
	Scope   string     `json:"scope"`
	Text    string     `json:"text"`
}

// annotationAuthor renders the subject as the "<kind>:<id>" reference store.Annotation.CreatedBy
// documents ("user:<name>", "token:<name>"); an anonymous subject -- what auth.mode=anonymous
// produces -- has no id to attribute.
func annotationAuthor(subject authz.Subject) string { //nolint:gocritic // Subject is a value type by design
	if subject.ID == "" || subject.Kind == authz.SubjectAnonymous {
		return string(authz.SubjectAnonymous)
	}
	return string(subject.Kind) + ":" + subject.ID
}

// annotationIDFrom resolves the {id} path parameter, answering 404 and
// reporting false for anything that is not a canonical UUID -- targetIDFrom's
// reasoning, verbatim.
func annotationIDFrom(w http.ResponseWriter, r *http.Request) (string, bool) {
	id := chi.URLParam(r, "id")
	if _, err := uuid.Parse(id); err != nil {
		writeProblem(w, http.StatusNotFound, "annotation not found", "no annotation with that id")
		return "", false
	}
	return id, true
}

// handleAnnotationsList serves one page of annotations, newest first, behind an opaque keyset
// cursor.
func (s *Server) handleAnnotationsList(w http.ResponseWriter, r *http.Request) {
	if s.annotationsUnavailable(w) {
		return
	}
	q := r.URL.Query()

	from, ok := parseEventsTime(w, q.Get("from"), "from")
	if !ok {
		return
	}
	to, ok := parseEventsTime(w, q.Get("to"), "to")
	if !ok {
		return
	}

	var scope *string
	if q.Has("scope") {
		v := q.Get("scope")
		scope = &v
	}

	cursor := q.Get("cursor")
	if cursor != "" {
		if _, _, _, err := store.DecodeUUIDCursor(cursor); err != nil {
			writeProblem(w, http.StatusBadRequest, "invalid cursor", "cursor is malformed or does not match this server")
			return
		}
	}

	page, err := s.annotations.ListAnnotations(r.Context(), store.AnnotationFilter{
		Scope:  scope,
		From:   from,
		To:     to,
		Cursor: cursor,
		Limit:  clampPageLimit(parsePageLimit(q.Get("limit"))),
	})
	if err != nil {
		// Logged, never surfaced: the driver error can carry a DSN or other
		// upstream detail that has no business in an HTTP response body.
		slog.Error("list annotations failed", "error", err)
		writeProblem(w, http.StatusBadGateway, "annotations unavailable", "failed to query annotations")
		return
	}

	out := make([]annotationResponse, 0, len(page.Annotations))
	for i := range page.Annotations {
		out = append(out, annotationResponseFrom(&page.Annotations[i]))
	}
	writeJSON(w, annotationsListResponse{Annotations: out, NextCursor: page.NextCursor})
}

// handleAnnotationsCreate pins one mark: 201 with a Location header naming the new row; validation
// is delegated to store.AnnotationInput.Validate rather than reimplemented here.
func (s *Server) handleAnnotationsCreate(w http.ResponseWriter, r *http.Request) {
	if s.annotationsUnavailable(w) {
		return
	}
	var req annotationRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeProblem(w, http.StatusBadRequest, "invalid request",
			`an annotation body must be JSON with "startAt" (RFC3339) and "text", plus optional "endAt" and "scope"`)
		return
	}

	subject, _ := SubjectFrom(r.Context())
	in := store.AnnotationInput{
		StartAt: req.StartAt, EndAt: req.EndAt, Scope: req.Scope, Text: req.Text,
		CreatedBy: annotationAuthor(subject),
	}
	if err := in.Validate(); err != nil {
		writeProblem(w, http.StatusUnprocessableEntity, "invalid annotation", publicValidationDetail(err))
		return
	}

	a, err := s.annotations.CreateAnnotation(r.Context(), in)
	if err != nil {
		// Validate ran above, so a validation error can only mean the store's
		// rules moved ahead of this package's copy of them -- report it as the
		// rejected value it is, not as a backend outage.
		if isAnnotationValidationError(err) {
			writeProblem(w, http.StatusUnprocessableEntity, "invalid annotation", publicValidationDetail(err))
			return
		}
		slog.Error("create annotation failed", "error", err)
		writeProblem(w, http.StatusBadGateway, "annotations unavailable", "failed to create the annotation")
		return
	}

	w.Header().Set("Location", "/api/v1/annotations/"+a.ID)
	writeJSONStatus(w, http.StatusCreated, annotationResponseFrom(&a))
}

// annotationValidationPrefix is the prefix store.AnnotationInput.Validate
// builds every one of its errors with. Validate returns plain errors, no
// sentinel, so the prefix is the only discriminator there is.
const annotationValidationPrefix = "store: annotation: "

// isAnnotationValidationError reports whether err came from store.AnnotationInput.Validate rather
// than from the database.
func isAnnotationValidationError(err error) bool {
	if errors.Is(err, store.ErrNotFound) || errors.Is(err, store.ErrAlreadyExists) || errors.Is(err, store.ErrInUse) {
		return false
	}
	return strings.HasPrefix(err.Error(), annotationValidationPrefix)
}

// handleAnnotationsDelete removes one mark. An unknown id and a malformed one
// are both 404 (annotationIDFrom), and deleting a mark that is not there is
// ErrNotFound rather than success -- the caller asked about a SPECIFIC mark.
func (s *Server) handleAnnotationsDelete(w http.ResponseWriter, r *http.Request) {
	if s.annotationsUnavailable(w) {
		return
	}
	id, ok := annotationIDFrom(w, r)
	if !ok {
		return
	}

	if err := s.annotations.DeleteAnnotation(r.Context(), id); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeProblem(w, http.StatusNotFound, "annotation not found", "no annotation with that id")
			return
		}
		slog.Error("delete annotation failed", "annotation", id, "error", err) //nolint:gosec // G706: structured slog fields, not string-built log injection
		writeProblem(w, http.StatusBadGateway, "annotations unavailable", "failed to delete the annotation")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
