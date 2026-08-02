package promql_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/EsDmitrii/kconmon-ng/internal/console/promql"
)

func guards() promql.Guards {
	return promql.Guards{QueryTimeout: 5 * time.Second, MaxRange: 24 * time.Hour, MaxResponseBytes: 1024}
}

const okBody = `{"status":"success","data":{"resultType":"vector","result":[]}}`

func TestQueryPassthrough(t *testing.T) {
	var gotForm string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/query" || r.Method != http.MethodPost {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		_ = r.ParseForm()
		gotForm = r.Form.Encode()
		_, _ = w.Write([]byte(okBody))
	}))
	defer srv.Close()

	raw, err := promql.New(srv.URL, guards()).Query(context.Background(), "up", time.Time{})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if string(raw) != okBody {
		t.Errorf("body not passed through: %s", raw)
	}
	if !strings.Contains(gotForm, "query=up") || strings.Contains(gotForm, "time=") {
		t.Errorf("form params wrong: %s", gotForm)
	}
}

func TestQueryRangeGuards(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(okBody))
	}))
	defer srv.Close()
	c := promql.New(srv.URL, guards())
	now := time.Now()

	if _, err := c.QueryRange(context.Background(), "up", now.Add(-25*time.Hour), now, time.Minute); !errors.Is(err, promql.ErrRangeTooLarge) {
		t.Errorf("expected ErrRangeTooLarge, got %v", err)
	}
	if _, err := c.QueryRange(context.Background(), "up", now, now.Add(-time.Hour), time.Minute); !errors.Is(err, promql.ErrBadRequest) {
		t.Errorf("expected ErrBadRequest for end<=start, got %v", err)
	}
	if _, err := c.QueryRange(context.Background(), "up", now.Add(-time.Hour), now, 0); !errors.Is(err, promql.ErrBadRequest) {
		t.Errorf("expected ErrBadRequest for step<=0, got %v", err)
	}
	if _, err := c.QueryRange(context.Background(), "up", now.Add(-time.Hour), now, 15*time.Second); err != nil {
		t.Errorf("valid range must pass: %v", err)
	}
}

func TestResponseSizeCap(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"status":"success","data":"` + strings.Repeat("x", 2048) + `"}`))
	}))
	defer srv.Close()

	_, err := promql.New(srv.URL, guards()).Query(context.Background(), "up", time.Time{})
	if !errors.Is(err, promql.ErrResponseTooLarge) {
		t.Fatalf("expected ErrResponseTooLarge, got %v", err)
	}
}

func TestUpstream4xxPassthrough(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"status":"error","errorType":"bad_data","error":"parse error"}`))
	}))
	defer srv.Close()

	_, err := promql.New(srv.URL, guards()).Query(context.Background(), "up{", time.Time{})
	var ue *promql.UpstreamError
	if !errors.As(err, &ue) || ue.Status != http.StatusBadRequest {
		t.Fatalf("expected UpstreamError 400, got %v", err)
	}
}
