package httpapi

import (
	"reflect"
	"strings"
	"testing"
)

// An allow-listed key the route's strict decoder refuses would only ever describe refused requests, so
// every key names a field of the body the route decodes.
func TestAuditAllowlistKeysAreFieldsOfTheRouteBody(t *testing.T) {
	bodies := map[string]any{
		"POST /api/v1/incidents":       incidentRequest{},
		"PATCH /api/v1/incidents/{id}": incidentPatchRequest{},
		"POST /api/v1/users":           userCreateRequest{},
		"PATCH /api/v1/users/{id}":     userPatchRequest{},
	}
	for route, body := range bodies {
		fields := map[string]bool{}
		typ := reflect.TypeOf(body)
		for field := range typ.Fields() {
			fields[strings.Split(field.Tag.Get("json"), ",")[0]] = true
		}
		for _, key := range auditDetailAllowlist[route] {
			if !fields[key] {
				t.Errorf("%s allow-lists %q, which its body %s does not have", route, key, typ.Name())
			}
		}
	}
}
