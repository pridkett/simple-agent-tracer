package httpapi_test

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pridkett/simple_agent_tracer/internal/auth"
	"github.com/pridkett/simple_agent_tracer/internal/httpapi"
)

func TestLocalBrowserAccess(t *testing.T) {
	fixture := newFixture(t, filepath.Join(t.TempDir(), "local.db"))
	access, _, err := auth.New(t.Context(), true, "", auth.OIDCConfig{})
	if err != nil {
		t.Fatal(err)
	}
	handler := httpapi.New(fixture.store, access, nil, nil)
	for _, tc := range []struct {
		name, method, host, peer, origin, fetchSite string
		want                                        int
	}{
		{"localhost", "GET", "localhost:4318", "127.0.0.1:42000", "", "", http.StatusOK},
		{"IPv6 loopback", "GET", "[::1]:4318", "[::1]:42000", "", "", http.StatusOK},
		{"same-origin write", "POST", "localhost:4318", "127.0.0.1:42000", "http://localhost:4318", "same-origin", http.StatusCreated},
		{"remote peer", "GET", "localhost:4318", "192.0.2.1:42000", "", "", http.StatusUnauthorized},
		{"nonlocal host", "GET", "site.example:4318", "127.0.0.1:42000", "", "", http.StatusUnauthorized},
		{"cross-site read", "GET", "localhost:4318", "127.0.0.1:42000", "", "cross-site", http.StatusUnauthorized},
		{"cross-origin read", "GET", "localhost:4318", "127.0.0.1:42000", "http://localhost:5173", "", http.StatusUnauthorized},
		{"cross-origin write", "POST", "localhost:4318", "127.0.0.1:42000", "http://site.example", "", http.StatusForbidden},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, "http://"+tc.host+"/api/v1/projects", strings.NewReader(`{"slug":"local-created"}`))
			req.RemoteAddr = tc.peer
			req.Header.Set("X-Forwarded-For", "127.0.0.1")
			req.Header.Set("Origin", tc.origin)
			req.Header.Set("Sec-Fetch-Site", tc.fetchSite)
			res := httptest.NewRecorder()
			handler.ServeHTTP(res, req)
			if res.Code != tc.want {
				t.Fatalf("got %d want %d: %s", res.Code, tc.want, res.Body.String())
			}
			if res.Header().Get("Set-Cookie") != "" {
				t.Fatal("local access set a session cookie")
			}
			statusReq := req.Clone(req.Context())
			statusReq.Method = http.MethodGet
			statusReq.URL.Path = "/auth/status"
			statusRes := httptest.NewRecorder()
			handler.ServeHTTP(statusRes, statusReq)
			status := readJSON[map[string]bool](t, statusRes)
			local := tc.want == http.StatusOK || tc.want == http.StatusCreated
			if statusRes.Code != http.StatusOK || status["local"] != local || status["oidc"] || status["signedIn"] {
				t.Fatalf("unexpected browser sign-in status: %s", statusRes.Body.String())
			}
		})
	}
}

func TestLocalModeRejectsOIDC(t *testing.T) {
	_, _, err := auth.New(t.Context(), true, "", auth.OIDCConfig{Issuer: "https://oidc.example"})
	if err == nil {
		t.Fatal("enabled local access alongside OIDC")
	}
}
