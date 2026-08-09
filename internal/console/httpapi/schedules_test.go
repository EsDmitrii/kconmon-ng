package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/EsDmitrii/kconmon-ng/internal/console/store"
)

// The ScheduleService half of fakeChecksStore (the DefinitionService half,
// and the struct itself, live in definitions_test.go -- one fake, so the
// FOREIGN KEY and the ON DELETE CASCADE between the two tables can be
// modelled at all).

func (f *fakeChecksStore) CreateSchedule(_ context.Context, in store.ScheduleInput) (store.Schedule, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := in.Validate(); err != nil {
		return store.Schedule{}, err
	}
	// The FK: a schedule for a definition that does not exist is ErrNotFound,
	// which is what the real *store.DB turns the constraint violation into.
	if _, ok := f.defs[in.DefinitionID]; !ok {
		return store.Schedule{}, store.ErrNotFound
	}
	now := time.Now().UTC()
	s := store.Schedule{
		ID: uuid.NewString(), DefinitionID: in.DefinitionID, Kind: in.Kind,
		IntervalNs: in.IntervalNs, RunAt: in.RunAt, Enabled: in.Enabled,
		NextFireAt: in.NextFireAt, CreatedAt: now, UpdatedAt: now,
	}
	f.scheds[s.ID] = s
	return s, nil
}

func (f *fakeChecksStore) UpdateSchedule(_ context.Context, id string, in store.ScheduleInput) (store.Schedule, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := in.Validate(); err != nil {
		return store.Schedule{}, err
	}
	existing, ok := f.scheds[id]
	if !ok {
		return store.Schedule{}, store.ErrNotFound
	}
	updated := store.Schedule{
		ID: id,
		// definition_id is deliberately NOT updatable (store's UpdateSchedule
		// query does not touch the column) -- modelled here so a handler that
		// tried to move a schedule would be caught by the fake, not silently
		// pass.
		DefinitionID: existing.DefinitionID,
		Kind:         in.Kind, IntervalNs: in.IntervalNs, RunAt: in.RunAt, Enabled: in.Enabled,
		LastFiredAt: existing.LastFiredAt, NextFireAt: in.NextFireAt,
		CreatedAt: existing.CreatedAt, UpdatedAt: time.Now().UTC(),
	}
	f.scheds[id] = updated
	return updated, nil
}

func (f *fakeChecksStore) DeleteSchedule(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.scheds[id]; !ok {
		return store.ErrNotFound
	}
	delete(f.scheds, id)
	return nil
}

func (f *fakeChecksStore) MarkScheduleFired(
	_ context.Context, id string, firedAt time.Time, nextFireAt *time.Time, lastError string,
) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.scheds[id]
	if !ok {
		return store.ErrNotFound
	}
	s.LastFiredAt = &firedAt
	s.NextFireAt = nextFireAt
	// Mirrors the real UPDATE's derivation: the stamp comes FROM the text, so
	// the fake cannot produce a pair the database would refuse to (#5).
	s.LastError = lastError
	if lastError == "" {
		s.LastErrorAt = nil
	} else {
		s.LastErrorAt = &firedAt
	}
	f.scheds[id] = s
	return nil
}

func (f *fakeChecksStore) GetSchedule(_ context.Context, id string) (store.Schedule, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.scheds[id]
	if !ok {
		return store.Schedule{}, store.ErrNotFound
	}
	return s, nil
}

func (f *fakeChecksStore) ListSchedules(_ context.Context, filter store.ScheduleFilter) (store.SchedulePage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]store.Schedule, 0, len(f.scheds))
	for _, s := range f.scheds {
		if filter.DefinitionID != "" && s.DefinitionID != filter.DefinitionID {
			continue
		}
		out = append(out, s)
	}
	return store.SchedulePage{Schedules: out}, nil
}

func (f *fakeChecksStore) ListDueSchedules(_ context.Context, due time.Time, limit int) ([]store.Schedule, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]store.Schedule, 0, len(f.scheds))
	for _, s := range f.scheds {
		if s.Enabled && s.NextFireAt != nil && !s.NextFireAt.After(due) {
			out = append(out, s)
		}
		if limit > 0 && len(out) == limit {
			break
		}
	}
	return out, nil
}

func scheduleRoutes(id string) []struct{ method, path string } {
	return []struct{ method, path string }{
		{http.MethodGet, "/api/v1/schedules"},
		{http.MethodPost, "/api/v1/schedules"},
		{http.MethodGet, "/api/v1/schedules/" + id},
		{http.MethodPut, "/api/v1/schedules/" + id},
		{http.MethodDelete, "/api/v1/schedules/" + id},
	}
}

// seedDefinition creates one definition through the real handler and returns
// its id, so every schedule test starts from a definition that genuinely
// exists behind the FK.
func seedDefinition(t *testing.T, s *Server) string {
	t.Helper()
	w := doRequest(t, s, http.MethodPost, "/api/v1/checks", strings.NewReader(validDefinitionBody), mutateWithCSRF)
	if w.Code != http.StatusCreated {
		t.Fatalf("seed definition = %d, want 201: %s", w.Code, w.Body)
	}
	var def definitionResponse
	if err := json.Unmarshal(w.Body.Bytes(), &def); err != nil {
		t.Fatalf("decode seed definition: %v", err)
	}
	return def.ID
}

func intervalScheduleBody(defID string, interval time.Duration, enabled bool) string {
	return fmt.Sprintf(`{"definitionId":%q,"kind":"interval","intervalNs":%d,"enabled":%t}`,
		defID, int64(interval), enabled)
}

func TestSchedulesWithoutStoreReturns503(t *testing.T) {
	s := newChecksTestServer(t, "operator", Deps{})
	body := intervalScheduleBody(uuid.NewString(), time.Minute, true)
	for _, c := range scheduleRoutes(uuid.NewString()) {
		var mutate func(*http.Request)
		if isMutatingMethod(c.method) {
			mutate = mutateWithCSRF
		}
		w := doRequest(t, s, c.method, c.path, strings.NewReader(body), mutate)
		if w.Code != http.StatusServiceUnavailable {
			t.Errorf("%s %s without a ScheduleService = %d, want 503: %s", c.method, c.path, w.Code, w.Body)
		}
		if !strings.Contains(w.Body.String(), "console.database.mode") {
			t.Errorf("%s %s 503 detail = %s, want it to name console.database.mode", c.method, c.path, w.Body)
		}
	}
}

func TestSchedulesCreateReturns201AndSeedsNextFire(t *testing.T) {
	st := newFakeChecksStore()
	s := newOperatorChecksServer(t, st, nil)
	defID := seedDefinition(t, s)

	before := time.Now().UTC()
	w := doRequest(t, s, http.MethodPost, "/api/v1/schedules",
		strings.NewReader(intervalScheduleBody(defID, 5*time.Minute, true)), mutateWithCSRF)
	if w.Code != http.StatusCreated {
		t.Fatalf("status %d, want 201: %s", w.Code, w.Body)
	}
	var got scheduleResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.DefinitionID != defID || got.Kind != "interval" || got.IntervalNs != int64(5*time.Minute) {
		t.Fatalf("body = %+v, want the created schedule echoed back", got)
	}
	if want := "/api/v1/schedules/" + got.ID; w.Header().Get("Location") != want {
		t.Errorf("Location = %q, want %q", w.Header().Get("Location"), want)
	}
	// Without a seeded next_fire_at the row would sit outside the due index
	// forever -- a schedule that silently never runs.
	if got.NextFireAt == nil || got.NextFireAt.Before(before) {
		t.Fatalf("nextFireAt = %v, want ~now so the first fire lands on the next tick", got.NextFireAt)
	}
}

// Decision 9's honest refusal: cron is a cadence this milestone consciously
// did not ship, and saying so beats a generic invalid-enum message that sends
// an operator hunting a typo that is not there.
func TestSchedulesCronReturns422WithTheMilestoneMessage(t *testing.T) {
	st := newFakeChecksStore()
	s := newOperatorChecksServer(t, st, nil)
	defID := seedDefinition(t, s)

	body := fmt.Sprintf(`{"definitionId":%q,"kind":"cron","enabled":true}`, defID)
	w := doRequest(t, s, http.MethodPost, "/api/v1/schedules", strings.NewReader(body), mutateWithCSRF)
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("kind=cron = %d, want 422: %s", w.Code, w.Body)
	}
	if !strings.Contains(w.Body.String(), "cron schedules land in a later milestone") {
		t.Fatalf("422 detail = %s, want the milestone message, not a generic enum error", w.Body)
	}
	// And specifically NOT the generic enum message store would produce.
	if strings.Contains(w.Body.String(), "must be one of once, interval, continuous") {
		t.Errorf("422 detail = %s, want cron handled before the enum check", w.Body)
	}
}

func TestSchedulesUnknownDefinitionReturns422(t *testing.T) {
	s := newOperatorChecksServer(t, newFakeChecksStore(), nil)
	missing := uuid.NewString()
	w := doRequest(t, s, http.MethodPost, "/api/v1/schedules",
		strings.NewReader(intervalScheduleBody(missing, time.Minute, true)), mutateWithCSRF)
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("schedule for an unknown definition = %d, want 422 -- the missing row is the BODY's, not the URL's: %s",
			w.Code, w.Body)
	}
	if !strings.Contains(w.Body.String(), missing) {
		t.Errorf("422 detail = %s, want it to name the definition id", w.Body)
	}
}

func TestSchedulesIntervalIsClampedToTheFloor(t *testing.T) {
	st := newFakeChecksStore()
	s := newOperatorChecksServer(t, st, nil)
	defID := seedDefinition(t, s)

	w := doRequest(t, s, http.MethodPost, "/api/v1/schedules",
		strings.NewReader(intervalScheduleBody(defID, time.Second, true)), mutateWithCSRF)
	if w.Code != http.StatusCreated {
		t.Fatalf("status %d, want 201: %s", w.Code, w.Body)
	}
	var got scheduleResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	// Clamped, and visibly so: the response is the STORED row, so a client
	// that asked for 1s is told in the very answer it reads that it got the
	// floor.
	if got.IntervalNs != int64(minScheduleInterval) {
		t.Fatalf("intervalNs = %d, want it clamped up to %d", got.IntervalNs, int64(minScheduleInterval))
	}

	// A zero interval is NOT clamped into existence -- it is store's own
	// "kind interval requires a positive interval".
	w = doRequest(t, s, http.MethodPost, "/api/v1/schedules",
		strings.NewReader(intervalScheduleBody(defID, 0, true)), mutateWithCSRF)
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("zero interval = %d, want 422: %s", w.Code, w.Body)
	}
}

func TestSchedulesOnceRequiresAFutureRunAt(t *testing.T) {
	st := newFakeChecksStore()
	s := newOperatorChecksServer(t, st, nil)
	defID := seedDefinition(t, s)

	past := time.Now().UTC().Add(-time.Hour).Format(time.RFC3339Nano)
	w := doRequest(t, s, http.MethodPost, "/api/v1/schedules",
		strings.NewReader(fmt.Sprintf(`{"definitionId":%q,"kind":"once","runAt":%q,"enabled":true}`, defID, past)),
		mutateWithCSRF)
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("runAt in the past = %d, want 422: %s", w.Code, w.Body)
	}

	future := time.Now().UTC().Add(time.Hour)
	w = doRequest(t, s, http.MethodPost, "/api/v1/schedules",
		strings.NewReader(fmt.Sprintf(`{"definitionId":%q,"kind":"once","runAt":%q,"enabled":true}`,
			defID, future.Format(time.RFC3339Nano))), mutateWithCSRF)
	if w.Code != http.StatusCreated {
		t.Fatalf("runAt in the future = %d, want 201: %s", w.Code, w.Body)
	}
	var got scheduleResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.NextFireAt == nil || !got.NextFireAt.Equal(future) {
		t.Fatalf("nextFireAt = %v, want it seeded to runAt %v", got.NextFireAt, future)
	}
}

// kind=continuous is agent-side: the scheduler loop skips it, so it must not
// be handed a next_fire_at that would invite a fire it has no concept of.
func TestSchedulesContinuousStaysOutOfTheDueIndex(t *testing.T) {
	st := newFakeChecksStore()
	s := newOperatorChecksServer(t, st, nil)
	defID := seedDefinition(t, s)

	w := doRequest(t, s, http.MethodPost, "/api/v1/schedules",
		strings.NewReader(fmt.Sprintf(`{"definitionId":%q,"kind":"continuous","enabled":true}`, defID)), mutateWithCSRF)
	if w.Code != http.StatusCreated {
		t.Fatalf("status %d, want 201: %s", w.Code, w.Body)
	}
	var got scheduleResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.NextFireAt != nil {
		t.Fatalf("nextFireAt = %v, want null for a continuous schedule", got.NextFireAt)
	}
	if got.IntervalNs != 0 {
		t.Fatalf("intervalNs = %d, want 0 -- continuous carries no cadence (store.ScheduleInput.Validate)", got.IntervalNs)
	}

	// And an interval ON a continuous schedule is refused, by store's own
	// cross-field rule -- see the deviation note in the task report.
	w = doRequest(t, s, http.MethodPost, "/api/v1/schedules",
		strings.NewReader(fmt.Sprintf(`{"definitionId":%q,"kind":"continuous","intervalNs":%d}`, defID, int64(time.Minute))),
		mutateWithCSRF)
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("continuous with an interval = %d, want 422: %s", w.Code, w.Body)
	}
}

func TestSchedulesMalformedAndUnknownIDReturn404(t *testing.T) {
	st := newFakeChecksStore()
	s := newOperatorChecksServer(t, st, nil)
	defID := seedDefinition(t, s)
	body := intervalScheduleBody(defID, time.Minute, true)

	for _, id := range []string{"not-a-uuid", uuid.NewString()} {
		for _, method := range []string{http.MethodGet, http.MethodPut, http.MethodDelete} {
			var mutate func(*http.Request)
			if isMutatingMethod(method) {
				mutate = mutateWithCSRF
			}
			w := doRequest(t, s, method, "/api/v1/schedules/"+id, strings.NewReader(body), mutate)
			if w.Code != http.StatusNotFound {
				t.Errorf("%s /api/v1/schedules/%s = %d, want 404: %s", method, id, w.Code, w.Body)
			}
		}
	}
}

func TestSchedulesUpdateRoundTripsAndRefusesADefinitionMove(t *testing.T) {
	st := newFakeChecksStore()
	s := newOperatorChecksServer(t, st, nil)
	defID := seedDefinition(t, s)

	w := doRequest(t, s, http.MethodPost, "/api/v1/schedules",
		strings.NewReader(intervalScheduleBody(defID, time.Minute, true)), mutateWithCSRF)
	var created scheduleResponse
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode create: %v", err)
	}

	w = doRequest(t, s, http.MethodPut, "/api/v1/schedules/"+created.ID,
		strings.NewReader(intervalScheduleBody(defID, time.Hour, false)), mutateWithCSRF)
	if w.Code != http.StatusOK {
		t.Fatalf("update = %d, want 200: %s", w.Code, w.Body)
	}
	var updated scheduleResponse
	if err := json.Unmarshal(w.Body.Bytes(), &updated); err != nil {
		t.Fatalf("decode update: %v", err)
	}
	if updated.IntervalNs != int64(time.Hour) || updated.Enabled {
		t.Fatalf("update body = %+v, want the stored row", updated)
	}

	// Moving a schedule to another definition is refused explicitly, never
	// accepted-and-ignored: store's SQL cannot move it, and a client that got
	// a 200 would believe it had.
	other := uuid.NewString()
	w = doRequest(t, s, http.MethodPut, "/api/v1/schedules/"+created.ID,
		strings.NewReader(intervalScheduleBody(other, time.Hour, true)), mutateWithCSRF)
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("update moving the definition = %d, want 422: %s", w.Code, w.Body)
	}

	// An omitted definitionId keeps the stored one.
	w = doRequest(t, s, http.MethodPut, "/api/v1/schedules/"+created.ID,
		strings.NewReader(`{"kind":"continuous","enabled":true}`), mutateWithCSRF)
	if w.Code != http.StatusOK {
		t.Fatalf("update with no definitionId = %d, want 200: %s", w.Code, w.Body)
	}
	if err := json.Unmarshal(w.Body.Bytes(), &updated); err != nil {
		t.Fatalf("decode update: %v", err)
	}
	if updated.DefinitionID != defID {
		t.Errorf("definitionId = %q, want the stored %q", updated.DefinitionID, defID)
	}
}

func TestSchedulesDeleteReturns204(t *testing.T) {
	st := newFakeChecksStore()
	s := newOperatorChecksServer(t, st, nil)
	defID := seedDefinition(t, s)

	w := doRequest(t, s, http.MethodPost, "/api/v1/schedules",
		strings.NewReader(intervalScheduleBody(defID, time.Minute, true)), mutateWithCSRF)
	var created scheduleResponse
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode create: %v", err)
	}

	w = doRequest(t, s, http.MethodDelete, "/api/v1/schedules/"+created.ID, nil, mutateWithCSRF)
	if w.Code != http.StatusNoContent {
		t.Fatalf("delete = %d, want 204: %s", w.Code, w.Body)
	}
	w = doRequest(t, s, http.MethodGet, "/api/v1/schedules/"+created.ID, nil, nil)
	if w.Code != http.StatusNotFound {
		t.Errorf("get after delete = %d, want 404", w.Code)
	}
}

func TestSchedulesListFiltersByDefinition(t *testing.T) {
	st := newFakeChecksStore()
	s := newOperatorChecksServer(t, st, nil)
	defID := seedDefinition(t, s)

	w := doRequest(t, s, http.MethodPost, "/api/v1/schedules",
		strings.NewReader(intervalScheduleBody(defID, time.Minute, true)), mutateWithCSRF)
	if w.Code != http.StatusCreated {
		t.Fatalf("create = %d, want 201: %s", w.Code, w.Body)
	}

	w = doRequest(t, s, http.MethodGet, "/api/v1/schedules?definitionId="+defID, nil, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("list = %d, want 200: %s", w.Code, w.Body)
	}
	var page schedulesListResponse
	if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(page.Schedules) != 1 || page.Schedules[0].DefinitionID != defID {
		t.Fatalf("schedules = %+v, want exactly the one for %s", page.Schedules, defID)
	}

	w = doRequest(t, s, http.MethodGet, "/api/v1/schedules?definitionId="+uuid.NewString(), nil, nil)
	if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(page.Schedules) != 0 {
		t.Fatalf("schedules for an unrelated definition = %+v, want none", page.Schedules)
	}
}

// A viewer holds neither checks:read nor schedules:write, so every schedule
// route is 403 for it; operator holds both.
func TestSchedulesViewerIsForbiddenOperatorIsNot(t *testing.T) {
	body := intervalScheduleBody(uuid.NewString(), time.Minute, true)

	viewer := newChecksTestServer(t, "viewer", Deps{Definitions: newFakeChecksStore(), Schedules: newFakeChecksStore()})
	for _, c := range scheduleRoutes(uuid.NewString()) {
		var mutate func(*http.Request)
		if isMutatingMethod(c.method) {
			mutate = mutateWithCSRF
		}
		w := doRequest(t, viewer, c.method, c.path, strings.NewReader(body), mutate)
		if w.Code != http.StatusForbidden {
			t.Errorf("viewer %s %s = %d, want 403: %s", c.method, c.path, w.Code, w.Body)
		}
	}

	operator := newOperatorChecksServer(t, newFakeChecksStore(), nil)
	for _, c := range scheduleRoutes(uuid.NewString()) {
		var mutate func(*http.Request)
		if isMutatingMethod(c.method) {
			mutate = mutateWithCSRF
		}
		w := doRequest(t, operator, c.method, c.path, strings.NewReader(body), mutate)
		if w.Code == http.StatusForbidden || w.Code == http.StatusUnauthorized {
			t.Errorf("operator %s %s = %d, want to pass authorization: %s", c.method, c.path, w.Code, w.Body)
		}
	}
}

func TestSchedulesInvalidBodyReturns400(t *testing.T) {
	s := newOperatorChecksServer(t, newFakeChecksStore(), nil)
	w := doRequest(t, s, http.MethodPost, "/api/v1/schedules", strings.NewReader(`not json`), mutateWithCSRF)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("unparseable body = %d, want 400: %s", w.Code, w.Body)
	}
}

// TestSchedulesListBadInputsAre400 mirrors the checks-list guard: a garbage
// cursor or definitionId filter is the client's 400, never a 502.
func TestSchedulesListBadInputsAre400(t *testing.T) {
	st := newFakeChecksStore()
	s := newOperatorChecksServer(t, st, nil)

	w := doRequest(t, s, http.MethodGet, "/api/v1/schedules?cursor=garbage", nil, nil)
	if w.Code != http.StatusBadRequest {
		t.Errorf("garbage cursor = %d, want 400", w.Code)
	}
	w = doRequest(t, s, http.MethodGet, "/api/v1/schedules?definitionId=not-a-uuid", nil, nil)
	if w.Code != http.StatusBadRequest {
		t.Errorf("garbage definitionId = %d, want 400 (M4 Task 4 fix pass)", w.Code)
	}
}

/* ── QA round 5, finding #5: the failing schedule says so on the wire ─────── */

// TestSchedulesExposeTheLastError pins both halves of the DTO: the pair is
// PRESENT and empty on a healthy schedule (a client needs "" to mean healthy,
// not a missing key it cannot distinguish from an older server), and it
// carries the scheduler's own text once a fire has failed.
func TestSchedulesExposeTheLastError(t *testing.T) {
	st := newFakeChecksStore()
	s := newOperatorChecksServer(t, st, nil)
	defID := seedDefinition(t, s)

	w := doRequest(t, s, http.MethodPost, "/api/v1/schedules",
		strings.NewReader(intervalScheduleBody(defID, 5*time.Minute, true)), mutateWithCSRF)
	if w.Code != http.StatusCreated {
		t.Fatalf("create = %d, want 201: %s", w.Code, w.Body)
	}
	var created scheduleResponse
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if created.LastError != "" || created.LastErrorAt != nil {
		t.Errorf("a fresh schedule reports %q/%v, want an empty pair", created.LastError, created.LastErrorAt)
	}
	// The KEY has to be there even when empty.
	var raw map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decode raw: %v", err)
	}
	for _, key := range []string{"lastError", "lastErrorAt"} {
		if _, ok := raw[key]; !ok {
			t.Errorf("%q is absent from the response; a client cannot tell healthy from unsupported", key)
		}
	}

	// A failed fire, recorded exactly the way the scheduler records one.
	fired := time.Now().UTC()
	if err := st.MarkScheduleFired(context.Background(), created.ID, fired, &fired,
		"get definition "+defID+": store: not found"); err != nil {
		t.Fatalf("MarkScheduleFired: %v", err)
	}

	w = doRequest(t, s, http.MethodGet, "/api/v1/schedules/"+created.ID, nil, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("get = %d, want 200: %s", w.Code, w.Body)
	}
	var got scheduleResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !strings.Contains(got.LastError, "store: not found") {
		t.Errorf("lastError = %q, want the scheduler's own text", got.LastError)
	}
	if got.LastErrorAt == nil {
		t.Error("lastErrorAt is null beside a non-empty lastError")
	}

	// …and the listing carries it too: the Schedules tab reads that, not /{id}.
	w = doRequest(t, s, http.MethodGet, "/api/v1/schedules", nil, nil)
	var list schedulesListResponse
	if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if len(list.Schedules) != 1 || list.Schedules[0].LastError == "" {
		t.Errorf("list = %+v, want the failure on the listed row", list.Schedules)
	}
}
