package httpapi_test

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
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

func TestQueryOAuth(t *testing.T) {
	const origin = "https://tracer.example"
	const mcpPath = root + "/mcp"
	const resource = origin + mcpPath
	const metadataPath = auth.ResourceMetadataPrefix + mcpPath
	const requestBody = `{"from":"2026-01-01T00:00:00Z","to":"2026-01-02T00:00:00Z"}`

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	idp := httptest.NewTLSServer(mux)
	defer idp.Close()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer": idp.URL, "authorization_endpoint": idp.URL + "/authorize", "token_endpoint": idp.URL + "/token",
			"jwks_uri": idp.URL + "/jwks", "id_token_signing_alg_values_supported": []string{"RS256"},
		})
	})
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: &key.PublicKey, Algorithm: "RS256", Use: "sig"}}})
	})
	ctx := oidc.ClientContext(t.Context(), idp.Client())
	config := auth.OIDCConfig{Issuer: idp.URL, Client: "browser-client", Callback: origin + "/auth/callback", QueryOAuth: true}
	credentials := fmt.Sprintf(`[{"token":%q,"access":"read","project":"default"}]`, readerToken)
	access, _, err := auth.New(ctx, false, credentials, config)
	if err != nil {
		t.Fatal(err)
	}
	store, err := storage.Open(ctx, filepath.Join(t.TempDir(), "oauth.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err := store.CreateProject(ctx, "other", 30, true); err != nil {
		t.Fatal(err)
	}
	handler := httpapi.New(store, access, nil, nil)
	request := func(method, path, token, body string, want int) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequestWithContext(ctx, method, "https://proxy.internal"+path, strings.NewReader(body))
		req.Header.Set("Accept", "application/json, text/event-stream")
		req.Header.Set("Content-Type", "application/json")
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		if w.Code != want {
			t.Fatalf("%s %s got %d, want %d: %s", method, path, w.Code, want, w.Body.String())
		}
		return w
	}
	sign := func(typ string, changes map[string]any) string {
		t.Helper()
		signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: key}, (&jose.SignerOptions{}).WithType(jose.ContentType(typ)))
		if err != nil {
			t.Fatal(err)
		}
		claims := map[string]any{
			"iss": idp.URL, "aud": resource, "sub": "pocket-id-user", "scope": "openid traces:read",
			"exp": time.Now().Add(time.Minute).Unix(),
		}
		for field, value := range changes {
			claims[field] = value
		}
		raw, _ := json.Marshal(claims)
		jws, err := signer.Sign(raw)
		if err != nil {
			t.Fatal(err)
		}
		token, err := jws.CompactSerialize()
		if err != nil {
			t.Fatal(err)
		}
		return token
	}
	metadata := readJSON[auth.QueryResourceMetadata](t, request("GET", metadataPath, "", "", 200))
	if metadata.Resource != resource || len(metadata.AuthorizationServers) != 1 || metadata.AuthorizationServers[0] != idp.URL ||
		len(metadata.ScopesSupported) != 1 || metadata.ScopesSupported[0] != "traces:read" {
		t.Fatalf("unexpected resource metadata: %+v", metadata)
	}
	request("GET", strings.Replace(metadataPath, "/default/", "/unknown/", 1), "", "", 404)
	const list = `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`
	unauthorized := request("POST", mcpPath, "", list, 401)
	challenge := unauthorized.Header().Get("WWW-Authenticate")
	if challenge != `Bearer resource_metadata="`+origin+metadataPath+`", scope="traces:read"` {
		t.Fatalf("unexpected challenge: %s", challenge)
	}
	token := sign("at+jwt", nil)
	tools := request("POST", mcpPath, token, list, 200).Body.String()
	if !strings.Contains(tools, `"name":"query_model_calls"`) || !strings.Contains(tools, `"securitySchemes":[{"scopes":["traces:read"],"type":"oauth2"}]`) {
		t.Fatalf("unexpected MCP tools: %s", tools)
	}
	call := `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"query_model_calls","arguments":` + requestBody + `}}`
	if body := request("POST", mcpPath, token, call, 200).Body.String(); !strings.Contains(body, `"rows":[]`) {
		t.Fatalf("unexpected MCP call result: %s", body)
	}
	request("POST", root+"/query", token, requestBody, 200)
	request("POST", root+"/query", sign("at+jwt", map[string]any{"aud": []string{resource, idp.URL}}), requestBody, 200)
	prefix, _, _ := strings.Cut(token, ".")
	_, tail, _ := strings.Cut(sign("at+jwt", map[string]any{"scope": "traces:reader"}), ".")
	payload, _, _ := strings.Cut(tail, ".")
	_, originalTail, _ := strings.Cut(token, ".")
	_, signature, _ := strings.Cut(originalTail, ".")
	request("POST", root+"/query", prefix+"."+payload+"."+signature, requestBody, 401)
	request("POST", root+"/query/export", token, requestBody, 200)
	request("POST", root+"/query", readerToken, requestBody, 200)
	request("POST", "/api/v1/projects/other/query", token, requestBody, 401)
	request("PATCH", root, token, `{"captureContent":false}`, 403)
	request("POST", "/v1/traces", token, "", 401)
	for _, tc := range []struct {
		name, typ string
		claims    map[string]any
		status    int
	}{
		{"wrong issuer", "at+jwt", map[string]any{"iss": "https://untrusted.example"}, 401},
		{"ID token", "JWT", nil, 401},
		{"wrong audience", "at+jwt", map[string]any{"aud": "browser-client"}, 401},
		{"multiple audiences", "at+jwt", map[string]any{"aud": []string{resource, origin + "/api/v1/projects/other/mcp"}}, 401},
		{"expired", "at+jwt", map[string]any{"exp": time.Now().Add(-time.Minute).Unix()}, 401},
		{"no expiry", "at+jwt", map[string]any{"exp": nil}, 401},
		{"no subject", "at+jwt", map[string]any{"sub": ""}, 401},
		{"missing scope", "at+jwt", map[string]any{"scope": "traces:reader"}, 403},
	} {
		t.Run(tc.name, func(t *testing.T) {
			denied := request("POST", root+"/query", sign(tc.typ, tc.claims), requestBody, tc.status)
			want := "invalid_token"
			if tc.status == 403 {
				want = "insufficient_scope"
			}
			if !strings.Contains(denied.Header().Get("WWW-Authenticate"), `error="`+want+`"`) {
				t.Fatalf("unexpected challenge: %v", denied.Header())
			}
		})
	}
	config.QueryOAuth = false
	disabled, _, err := auth.New(ctx, false, credentials, config)
	if err != nil {
		t.Fatal(err)
	}
	handler = httpapi.New(store, disabled, nil, nil)
	request("GET", metadataPath, "", "", 404)
	request("POST", root+"/query", token, requestBody, 401)
}
