package authn

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/oauth2"

	"github.com/EsDmitrii/kconmon-ng/internal/console/cache"
)

// A sealed state carries its own expiry, since no record lapses with it any more.
func TestOIDCCallbackRefusesAnExpiredStateWithoutAskingTheIdP(t *testing.T) {
	var tokenRequests atomic.Int32
	idp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		tokenRequests.Add(1)
		http.Error(w, "unexpected", http.StatusBadRequest)
	}))
	t.Cleanup(idp.Close)
	kv := cache.NewInProcessKV()
	t.Cleanup(kv.Close)
	stateAEAD, err := newStateAEAD("console-secret")
	if err != nil {
		t.Fatalf("newStateAEAD: %v", err)
	}
	a := &OIDCAuthenticator{
		oauth2Config: oauth2.Config{ClientID: "c", Endpoint: oauth2.Endpoint{TokenURL: idp.URL}},
		stateAEAD:    stateAEAD,
		kv:           kv,
	}

	state, err := a.sealState(oidcState{Verifier: strings.Repeat("v", oidcVerifierLen), ReturnTo: "/", Expires: time.Now().Add(-time.Second).Unix()})
	if err != nil {
		t.Fatalf("sealState: %v", err)
	}
	if _, _, err := a.Callback(context.Background(), state, "code"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("Callback with an expired state: err = %v, want ErrInvalid", err)
	}
	if got := tokenRequests.Load(); got != 0 {
		t.Fatalf("token requests = %d, want 0", got)
	}
}
