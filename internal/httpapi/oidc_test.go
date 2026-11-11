package httpapi_test

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/go-jose/go-jose/v4"
	"github.com/pridkett/simple_agent_tracer/internal/auth"
	"github.com/pridkett/simple_agent_tracer/internal/httpapi"
	"github.com/pridkett/simple_agent_tracer/internal/storage"
)

func TestOIDCOwner(t *testing.T) {
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
		raw, _ := json.Marshal(map[string]any{"iss": issuer.URL, "sub": subject, "aud": "client", "exp": time.Now().Add(time.Minute).Unix(), "iat": time.Now().Unix(), "nonce": nonce})
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
	creds := `[{"access":"ingest","project":"default","source":"fixture","token":"` + agentToken + `"}]`
	access, _, err := auth.New(ctx, false, creds, auth.OIDCConfig{
		Issuer: issuer.URL, Client: "client", Secret: "fixture", Subject: "owner", Callback: origin + "/auth/callback",
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
		req := httptest.NewRequestWithContext(ctx, method, origin+path, strings.NewReader(`{"slug":"oidc-created"}`))
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
	login := func(subject string, want int, validState bool) *httptest.ResponseRecorder {
		res := request("GET", "/auth/login", "", nil, 303)
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
		cookies := res.Result().Cookies()
		if len(cookies) != 1 || !cookies[0].Secure || !cookies[0].HttpOnly || cookies[0].Path != "/" || cookies[0].Name != "__Host-tracer" {
			t.Fatalf("unexpected OIDC state cookie: %v", cookies)
		}
		values := url.Values{"state": {state}, "code": {subject + ":" + nonce}}
		return request("GET", "/auth/callback?"+values.Encode(), "", cookies[0], want)
	}
	login("owner", 400, false)
	login("visitor", 403, true)
	result := login("owner", 303, true)
	cookies := result.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatal("missing owner session cookie")
	}
	owner := cookies[0]
	request("GET", "/api/v1/projects", "", owner, 200)
	request("POST", "/api/v1/projects", "https://other.example", owner, 403)
	request("POST", "/api/v1/projects", "", owner, 403)
	request("POST", "/api/v1/projects", origin, owner, 201)
	request("POST", "/auth/logout", origin, owner, 204)
	request("GET", "/api/v1/projects", "", owner, 401)
}
