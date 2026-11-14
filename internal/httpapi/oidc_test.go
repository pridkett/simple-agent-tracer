package httpapi_test

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/go-jose/go-jose/v4"
	"github.com/pridkett/simple_agent_tracer/internal/auth"
	"github.com/pridkett/simple_agent_tracer/internal/httpapi"
	"github.com/pridkett/simple_agent_tracer/internal/storage"
)

func TestOIDCAdministrator(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: key}, nil)
	if err != nil {
		t.Fatal(err)
	}
	const origin = "https://tracer.example"
	mux := http.NewServeMux()
	issuer := httptest.NewTLSServer(mux)
	defer issuer.Close()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer": issuer.URL, "authorization_endpoint": issuer.URL + "/authorize", "token_endpoint": issuer.URL + "/token",
			"jwks_uri": issuer.URL + "/jwks", "id_token_signing_alg_values_supported": []string{"RS256"},
		})
	})
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: &key.PublicKey, Algorithm: "RS256", Use: "sig"}}})
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		if r.ParseForm() != nil || r.Form.Get("code_verifier") == "" {
			w.WriteHeader(400)
			return
		}
		subject, nonce, _ := strings.Cut(r.Form.Get("code"), ":")
		claims := map[string]any{"iss": issuer.URL, "sub": subject, "aud": "client", "exp": time.Now().Add(time.Minute).Unix(), "iat": time.Now().Unix(), "nonce": nonce}
		switch subject {
		case "default-member":
			claims["groups"] = []string{"admin"}
		case "custom-member":
			claims["groups"] = []string{"readers", "tracer-admins"}
		case "wrong-case":
			claims["groups"] = []string{"Admin", "Tracer-Admins"}
		case "unverified-member":
			claims["groups"] = []string{"admin", "tracer-admins"}
			claims["email_verified"] = false
		case "bad-groups":
			claims["groups"] = "admin"
		}
		raw, _ := json.Marshal(claims)
		signed, err := signer.Sign(raw)
		if err != nil {
			w.WriteHeader(500)
			return
		}
		id, err := signed.CompactSerialize()
		if err != nil {
			w.WriteHeader(500)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"access_token": "fixture", "token_type": "Bearer", "id_token": id})
	})
	ctx := oidc.ClientContext(t.Context(), issuer.Client())
	for _, tc := range []struct {
		name, group, legacySubject, allowed, denied string
	}{
		{"default group", "", "", "default-member", "custom-member"},
		{"configured group", "tracer-admins", "legacy-owner", "custom-member", "default-member"},
		{"legacy subject", "", "legacy-owner", "legacy-owner", "default-member"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			access, _, err := auth.New(ctx, false, "", auth.OIDCConfig{
				Issuer: issuer.URL, Client: "client", Secret: "fixture", AdminGroup: tc.group, LegacyOwnerSubject: tc.legacySubject, Callback: origin + "/auth/callback",
			})
			if err != nil {
				t.Fatal(err)
			}
			store, err := storage.Open(ctx, filepath.Join(t.TempDir(), "fixture.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			handler := httpapi.New(store, access, nil, nil)
			request := func(method, path, from string, cookie *http.Cookie, want int) *httptest.ResponseRecorder {
				t.Helper()
				body := `{"slug":"oidc-created"}`
				if strings.HasSuffix(path, "/ingest-keys") {
					body = `{"source":"oidc-exporter"}`
				}
				req := httptest.NewRequestWithContext(ctx, method, origin+path, strings.NewReader(body))
				if from != "" {
					req.Header.Set("Origin", from)
				}
				if cookie != nil {
					req.AddCookie(cookie)
				}
				res := httptest.NewRecorder()
				handler.ServeHTTP(res, req)
				if res.Code != want {
					t.Fatalf("%s %s: got %d want %d: %s", method, path, res.Code, want, res.Body.String())
				}
				return res
			}
			request("GET", "/api/v1/projects", "", nil, 401)
			signedOut := readJSON[map[string]bool](t, request("GET", "/auth/status", "", nil, 200))
			if !signedOut["oidc"] || signedOut["local"] || signedOut["signedIn"] {
				t.Fatalf("unexpected signed-out status: %v", signedOut)
			}
			login := func(subject string, want int, validState bool, returnTo string) *httptest.ResponseRecorder {
				params := url.Values{"returnTo": {returnTo}}
				res := request("GET", "/auth/login?"+params.Encode(), "", nil, 303)
				redirect, err := url.Parse(res.Header().Get("Location"))
				if err != nil {
					t.Fatal(err)
				}
				query := redirect.Query()
				state, nonce := query.Get("state"), query.Get("nonce")
				if state == "" || nonce == "" || query.Get("code_challenge_method") != "S256" {
					t.Fatal("incomplete OIDC request")
				}
				if !validState {
					state = "mismatch"
				}
				scopes := strings.Fields(query.Get("scope"))
				for _, scope := range []string{"openid", "email", "profile", "groups"} {
					if !slices.Contains(scopes, scope) {
						t.Fatalf("missing OIDC scope %s", scope)
					}
				}
				cookies := res.Result().Cookies()
				if len(cookies) != 1 || !cookies[0].Secure || !cookies[0].HttpOnly || cookies[0].Path != "/" || cookies[0].Name != "__Host-tracer" {
					t.Fatalf("unexpected OIDC state cookie: %v", cookies)
				}
				values := url.Values{"state": {state}, "code": {subject + ":" + nonce}}
				return request("GET", "/auth/callback?"+values.Encode(), "", cookies[0], want)
			}
			login(tc.allowed, 400, false, "")
			login(tc.denied, 403, true, "")
			for _, denied := range []string{"visitor", "wrong-case", "unverified-member", "bad-groups"} {
				login(denied, 403, true, "")
			}
			if tc.group != "" {
				login("legacy-owner", 403, true, "")
			}
			const spanURL = "/projects/default/traces/01010101010101010101010101010101/spans/0202020202020202"
			const sessionURL = "/projects/default/sessions/session?span=01010101010101010101010101010101.0202020202020202"
			for _, tcURL := range []struct{ target, want string }{
				{"", "/"},
				{spanURL, spanURL},
				{sessionURL, sessionURL},
				{"https://example.org/projects/default/traces", "/"},
				{"//example.org/projects/default/traces", "/"},
				{"/projects/%5c%5cexample.org", "/"},
				{"/projects/%2e%2e/auth/login", "/"},
			} {
				res := login(tc.allowed, 303, true, tcURL.target)
				if got := res.Header().Get("Location"); got != tcURL.want {
					t.Errorf("returnTo %q redirected to %q, want %q", tcURL.target, got, tcURL.want)
				}
			}
			result := login(tc.allowed, 303, true, "")
			cookies := result.Result().Cookies()
			if len(cookies) != 1 {
				t.Fatal("missing owner session cookie")
			}
			owner := cookies[0]
			signedIn := readJSON[map[string]bool](t, request("GET", "/auth/status", "", owner, 200))
			if !signedIn["oidc"] || signedIn["local"] || !signedIn["signedIn"] {
				t.Fatalf("unexpected signed-in status: %v", signedIn)
			}
			request("GET", "/api/v1/projects", "", owner, 200)
			request("POST", "/api/v1/projects", "https://other.example", owner, 403)
			request("POST", "/api/v1/projects", "", owner, 403)
			request("POST", "/api/v1/projects", origin, owner, 201)
			keys := "/api/v1/projects/oidc-created/ingest-keys"
			request("GET", keys, "", nil, 401)
			request("POST", keys, "https://other.example", owner, 403)
			request("POST", keys, "", owner, 403)
			request("POST", keys, origin, owner, 201)
			request("GET", keys, "", owner, 200)
			request("POST", "/auth/logout", origin, owner, 204)
			request("GET", "/api/v1/projects", "", owner, 401)
		})
	}
}
