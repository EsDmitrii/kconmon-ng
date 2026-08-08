package httpapi

import (
	"net/http"
	"os"
	"sort"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"gopkg.in/yaml.v3"

	"github.com/EsDmitrii/kconmon-ng/internal/console/authz"
)

// openapiSpecPath is docs/console-api.yaml relative to this package's
// directory -- the same "walk up to the repo root" form every other
// file-relative path in this repo's tests uses. `go test` always runs with
// the package directory as its working directory, so this is stable
// regardless of where the command was invoked from.
const openapiSpecPath = "../../../docs/console-api.yaml"

// openapiMethods is the closed set of OpenAPI path-item keys that denote an
// operation. Every OTHER key a path item may legally carry -- summary,
// description, servers, parameters, $ref, and any x- extension -- is not a
// method and must be skipped, or the spec side of the join would invent
// routes like "PARAMETERS /api/v1/targets/{id}" and fail against a router
// that is perfectly in sync.
var openapiMethods = map[string]string{
	"get":     http.MethodGet,
	"put":     http.MethodPut,
	"post":    http.MethodPost,
	"delete":  http.MethodDelete,
	"options": http.MethodOptions,
	"head":    http.MethodHead,
	"patch":   http.MethodPatch,
	"trace":   http.MethodTrace,
}

// TestEveryAPIRouteIsInTheOpenAPISpec walks the LIVE chi router the same way
// TestEveryAPIRouteHasAPermissionDecision does and fails if any registered
// /api/v1/* pattern is absent from docs/console-api.yaml, or if the spec
// names a path the router does not serve. Two hand-maintained artefacts with
// a machine-checked join beat one with none (Plan Decision 4). chi patterns
// use {id}; OpenAPI uses {id} too, so no translation is needed.
//
// Scope, both directions:
//
//   - router -> spec covers /api/v1/* ONLY. /healthz, /readyz, /metrics and
//     /ws are deliberately outside the spec (the first three are probe/scrape
//     endpoints, /ws is a protocol documented in WEBSOCKET.md, not a REST
//     path), so requiring them here would only force noise into the document.
//   - spec -> router covers EVERY path the document declares, /api/v1/* or
//     not. That is what makes a typo'd or removed path fail: the check is
//     against the full set of registered routes, so documenting /ws or
//     /healthz would still pass, while /api/v1/bogus (or a /api/v1/targets
//     that lost its DELETE in server.go) fails immediately.
//
// Methods are part of the key on both sides, so a path that is present but
// missing one of its verbs is drift and fails like a missing path does.
//
// The YAML is parsed with gopkg.in/yaml.v3 into map[string]any rather than
// with an OpenAPI library: the join needs path keys and method keys, nothing
// else, and a schema-validating dependency would buy nothing this test asks
// for while adding one more thing to keep current.
func TestEveryAPIRouteIsInTheOpenAPISpec(t *testing.T) {
	// Same server construction as TestEveryAPIRouteHasAPermissionDecision --
	// the whole point is to walk the router production actually builds. No
	// request is issued here, so the authenticator/policy pair only has to
	// exist for NewServer to wire the authenticated Group at all.
	authr := fakeAuthenticator{subject: authz.Subject{Kind: authz.SubjectUser, ID: "u1"}, mode: "local"}
	s := newAuthzServer(t, authr, authz.NewPolicy(nil), Deps{})

	registered := map[string]bool{} // every route the router serves
	apiRoutes := map[string]bool{}  // the /api/v1/* subset the spec must cover
	err := chi.Walk(s.router, func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		key := method + " " + route
		registered[key] = true
		if strings.HasPrefix(route, "/api/v1/") {
			apiRoutes[key] = true
		}
		return nil
	})
	if err != nil {
		t.Fatalf("chi.Walk: %v", err)
	}
	if len(apiRoutes) == 0 {
		t.Fatal("chi.Walk found no /api/v1/* routes -- the walk itself is broken, not the spec")
	}

	documented := readOpenAPIOperations(t)

	for _, key := range sortedKeys(apiRoutes) {
		if !documented[key] {
			t.Errorf("%s is registered in server.go but absent from %s -- document it (path + method)",
				key, openapiSpecPath)
		}
	}
	for _, key := range sortedKeys(documented) {
		if !registered[key] {
			t.Errorf("%s is declared in %s but the router serves no such route -- remove it or fix the path/method",
				key, openapiSpecPath)
		}
	}
}

// readOpenAPIOperations returns the spec's operations as a set of
// "METHOD /path" keys, the same shape chi.Walk yields.
func readOpenAPIOperations(t *testing.T) map[string]bool {
	t.Helper()

	raw, err := os.ReadFile(openapiSpecPath)
	if err != nil {
		t.Fatalf("read %s: %v", openapiSpecPath, err)
	}
	var doc map[string]any
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse %s: %v", openapiSpecPath, err)
	}

	paths, ok := doc["paths"].(map[string]any)
	if !ok {
		t.Fatalf("%s has no \"paths\" mapping", openapiSpecPath)
	}

	out := make(map[string]bool, len(paths))
	for path, item := range paths {
		operations, ok := item.(map[string]any)
		if !ok {
			t.Errorf("%s: path %q is not a mapping", openapiSpecPath, path)
			continue
		}
		for key := range operations {
			method, isMethod := openapiMethods[key]
			if !isMethod {
				continue // summary/parameters/$ref/x-* -- not an operation
			}
			out[method+" "+path] = true
		}
	}
	return out
}

// sortedKeys makes both failure lists deterministic: map iteration order is
// random, and a gate whose output reorders itself between runs is one nobody
// can diff.
func sortedKeys(set map[string]bool) []string {
	keys := make([]string, 0, len(set))
	for k := range set {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
