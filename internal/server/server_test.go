package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The internal API is service-to-service traffic: one shared token, fail closed
// when none is configured. Nothing here is reachable without it.
func TestInternalAPIRefusesWithoutTheToken(t *testing.T) {
	t.Setenv("NANOSEC_BUS_INTERNAL_TOKEN", "s3cret")
	s := New(nil, "", "")

	cases := []struct {
		name  string
		token string
		want  int
	}{
		{"no token", "", http.StatusUnauthorized},
		{"wrong token", "nope", http.StatusUnauthorized},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/api/v1/internal/container-requests", strings.NewReader(`{"user_id":"u","agent_id":"a"}`))
			if tc.token != "" {
				req.Header.Set("Authorization", "Bearer "+tc.token)
			}
			rec := httptest.NewRecorder()
			s.srv.Handler.ServeHTTP(rec, req)
			if rec.Code != tc.want {
				t.Fatalf("status = %d, want %d", rec.Code, tc.want)
			}
		})
	}

	// A non-bearer scheme is refused as well.
	req := httptest.NewRequest(http.MethodPost, "/api/v1/internal/container-requests", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Basic s3cret")
	rec := httptest.NewRecorder()
	s.srv.Handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("basic scheme: status = %d, want 401", rec.Code)
	}
}

// Without a configured token the API refuses every call instead of opening up.
func TestInternalAPIFailsClosedWithoutAToken(t *testing.T) {
	t.Setenv("NANOSEC_BUS_INTERNAL_TOKEN", "")
	s := New(nil, "", "")

	req := httptest.NewRequest(http.MethodPost, "/api/v1/internal/containers/u1/dispose", nil)
	rec := httptest.NewRecorder()
	s.srv.Handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
}

// TestHealthzIsOpenWithoutTheToken: the probe carries no token.
func TestHealthzIsOpenWithoutTheToken(t *testing.T) {
	t.Setenv("NANOSEC_BUS_INTERNAL_TOKEN", "s3cret")
	s := New(nil, "", "")
	rec := httptest.NewRecorder()
	s.srv.Handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
}

// TestMetricsIsAnonymous: a scraper is infrastructure, not a caller. It answers on
// the internal port without a token, next to /healthz, and the rest of the API
// still refuses one.
func TestMetricsIsAnonymous(t *testing.T) {
	t.Setenv("NANOSEC_BUS_INTERNAL_TOKEN", "s3cret")
	s := New(nil, "runner-test", "")
	rec := httptest.NewRecorder()
	s.srv.Handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "provisioner_active_tunnels") {
		t.Fatalf("the exposition has no provisioner metric: %s", rec.Body.String()[:200])
	}

	rec = httptest.NewRecorder()
	s.srv.Handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/internal/console/runners", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("an unauthenticated API call returned %d, want 401", rec.Code)
	}
}
