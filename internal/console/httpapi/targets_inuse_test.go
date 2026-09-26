package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/EsDmitrii/kconmon-ng/internal/console/store"
)

// pagingChecksStore honours Limit and Cursor the way *store.DB does, which fakeChecksStore does not.
type pagingChecksStore struct {
	*fakeChecksStore
}

func (p pagingChecksStore) ListDefinitions(ctx context.Context, filter store.DefinitionFilter) (store.DefinitionPage, error) { //nolint:gocritic // hugeParam: test double mirrors the store signature
	all, err := p.fakeChecksStore.ListDefinitions(ctx, store.DefinitionFilter{TargetID: filter.TargetID, Enabled: filter.Enabled})
	if err != nil {
		return store.DefinitionPage{}, err
	}
	defs := all.Definitions
	sort.Slice(defs, func(i, j int) bool { return defs[i].ID < defs[j].ID })
	start := 0
	if filter.Cursor != "" {
		start, _ = strconv.Atoi(filter.Cursor)
	}
	end := min(start+filter.Limit, len(defs))
	page := store.DefinitionPage{Definitions: defs[start:end]}
	if end < len(defs) {
		page.NextCursor = strconv.Itoa(end)
	}
	return page, nil
}

// The 409 names ten referencing definitions and counts the rest: with 30 of them the operator must
// read "and 20 more", not "and 1 more".
func TestTargetsDeleteInUseCountsEveryReferencingDefinition(t *testing.T) {
	fake := newFakeTargetService()
	checks := newFakeChecksStore()
	s := newTargetsTestServer(t, "operator", fake, Deps{Definitions: pagingChecksStore{checks}})

	w := doRequest(t, s, http.MethodPost, "/api/v1/targets", strings.NewReader(validTargetBody), mutateWithCSRF)
	if w.Code != http.StatusCreated {
		t.Fatalf("create status %d: %s", w.Code, w.Body)
	}
	var created targetResponse
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode: %v", err)
	}
	fake.inUse[created.ID] = true
	checks.targets[created.ID] = true
	for i := range 30 {
		if _, err := checks.CreateDefinition(context.Background(), store.DefinitionInput{
			Name: fmt.Sprintf("edge-%02d", i), SourceSelection: "all", DestinationKind: "target",
			DestinationTargetID: created.ID, CheckType: "icmp", Plane: "pod", Enabled: true,
		}); err != nil {
			t.Fatalf("seed definition %d: %v", i, err)
		}
	}

	w = doRequest(t, s, http.MethodDelete, "/api/v1/targets/"+created.ID, nil, mutateWithCSRF)
	if w.Code != http.StatusConflict {
		t.Fatalf("status %d, want 409: %s", w.Code, w.Body)
	}
	detail := problemDetail(t, w.Body.Bytes())
	if !strings.Contains(detail, "and 20 more") {
		t.Errorf("409 detail = %q, want the ten listed names followed by \"and 20 more\"", detail)
	}
	for i := range 10 {
		if name := strconv.Quote(fmt.Sprintf("edge-%02d", i)); !strings.Contains(detail, name) {
			t.Errorf("409 detail = %q, want it to list %s", detail, name)
		}
	}
	if strings.Contains(detail, strconv.Quote("edge-10")) {
		t.Errorf("409 detail = %q, lists more than ten names", detail)
	}
}

// endlessChecksStore answers every ListDefinitions page with one more http definition on targetID
// and a next cursor, so a walk over it only ends at inUseDefinitionsMaxPages.
type endlessChecksStore struct {
	*fakeChecksStore
	targetID string
}

func (e endlessChecksStore) ListDefinitions(_ context.Context, filter store.DefinitionFilter) (store.DefinitionPage, error) { //nolint:gocritic // hugeParam: test double mirrors the store signature
	n, _ := strconv.Atoi(filter.Cursor)
	def := store.Definition{
		ID: fmt.Sprintf("def-%03d", n), Name: fmt.Sprintf("portal-%03d", n), SourceSelection: "all",
		DestinationKind: "target", DestinationTargetID: e.targetID, CheckType: "http", Plane: "pod", Enabled: true,
	}
	return store.DefinitionPage{Definitions: []store.Definition{def}, NextCursor: strconv.Itoa(n + 1)}, nil
}

// Both refusals that list definitions word the list the same way: a walk cut short reports its
// count as a lower bound, on the edit's 422 as on the delete's 409.
func TestTargetRefusalsCountATruncatedWalkAsALowerBound(t *testing.T) {
	fake := newFakeTargetService()
	target, err := fake.CreateTarget(context.Background(),
		store.TargetInput{Name: "portal", Kind: "url", Address: "https://svc.example.com/health"})
	if err != nil {
		t.Fatal(err)
	}
	s := newTargetsTestServer(t, "operator", fake, Deps{Definitions: endlessChecksStore{newFakeChecksStore(), target.ID}})
	const want = "and at least 10 more"

	w := doRequest(t, s, http.MethodPut, "/api/v1/targets/"+target.ID,
		strings.NewReader(`{"name":"portal","kind":"host","address":"svc.example.com:443"}`), mutateWithCSRF)
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("PUT url -> host = %d, want 422: %s", w.Code, w.Body)
	}
	if detail := problemDetail(t, w.Body.Bytes()); !strings.Contains(detail, want) {
		t.Errorf("422 detail = %q, want it to say %q", detail, want)
	}

	fake.inUse[target.ID] = true
	w = doRequest(t, s, http.MethodDelete, "/api/v1/targets/"+target.ID, nil, mutateWithCSRF)
	if w.Code != http.StatusConflict {
		t.Fatalf("DELETE = %d, want 409: %s", w.Code, w.Body)
	}
	if detail := problemDetail(t, w.Body.Bytes()); !strings.Contains(detail, want) {
		t.Errorf("409 detail = %q, want it to say %q", detail, want)
	}
}

// One referencing definition is "it", on the 409 as on the 422.
func TestTargetsDeleteInUseNamesASingleDefinitionInTheSingular(t *testing.T) {
	fake := newFakeTargetService()
	checks := newFakeChecksStore()
	s := newTargetsTestServer(t, "operator", fake, Deps{Definitions: checks})
	target, err := fake.CreateTarget(context.Background(),
		store.TargetInput{Name: "portal", Kind: "url", Address: "https://svc.example.com/health"})
	if err != nil {
		t.Fatal(err)
	}
	seedTargetDefinition(t, checks, httpDefinitionOn("portal-http", target.ID, true))
	fake.inUse[target.ID] = true

	w := doRequest(t, s, http.MethodDelete, "/api/v1/targets/"+target.ID, nil, mutateWithCSRF)
	if w.Code != http.StatusConflict {
		t.Fatalf("DELETE = %d, want 409: %s", w.Code, w.Body)
	}
	const want = `target "portal" is still referenced by check definition "portal-http"; delete or re-point it first`
	if detail := problemDetail(t, w.Body.Bytes()); detail != want {
		t.Errorf("409 detail = %q, want %q", detail, want)
	}
}
