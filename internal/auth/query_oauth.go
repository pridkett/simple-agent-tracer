package auth

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/go-jose/go-jose/v4"
	"golang.org/x/oauth2"
)

const QueryReadScope = "traces:read"
const ResourceMetadataPrefix = "/.well-known/oauth-protected-resource"

var projectSlug = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

var accessTokenAlgorithms = []jose.SignatureAlgorithm{
	jose.RS256, jose.RS384, jose.RS512, jose.PS256, jose.PS384, jose.PS512,
	jose.ES256, jose.ES384, jose.ES512, jose.EdDSA,
}

type queryOAuth struct {
	issuer, origin string
	verifier       *oidc.IDTokenVerifier
}

func newQueryOAuth(ctx context.Context, provider *oidc.Provider, issuer, origin string) (*queryOAuth, error) {
	var discovery struct {
		JWKS string `json:"jwks_uri"`
	}
	if err := provider.Claims(&discovery); err != nil || !httpsEndpoint(discovery.JWKS) {
		return nil, errors.New("query OAuth requires an HTTPS OIDC signing-key URL")
	}
	client := http.Client{Timeout: 5 * time.Second}
	if configured, ok := ctx.Value(oauth2.HTTPClient).(*http.Client); ok && configured != nil {
		client.Transport = configured.Transport
	}
	client.CheckRedirect = func(r *http.Request, via []*http.Request) error {
		if len(via) >= 10 || !httpsEndpoint(r.URL.String()) {
			return errors.New("invalid signing-key redirect")
		}
		return nil
	}
	algorithms := make([]string, 0, len(accessTokenAlgorithms))
	for _, algorithm := range accessTokenAlgorithms {
		algorithms = append(algorithms, string(algorithm))
	}
	return &queryOAuth{issuer: issuer, origin: origin, verifier: provider.VerifierContext(oidc.ClientContext(ctx, &client), &oidc.Config{
		// The expected audience is the URL of the requested project.
		SkipClientIDCheck:    true,
		SupportedSigningAlgs: algorithms,
	})}, nil
}

func httpsEndpoint(value string) bool {
	u, err := url.Parse(value)
	return err == nil && u.Scheme == "https" && u.Hostname() != "" && u.User == nil && u.Fragment == ""
}

type QueryResourceMetadata struct {
	Resource               string   `json:"resource"`
	AuthorizationServers   []string `json:"authorization_servers"`
	ScopesSupported        []string `json:"scopes_supported"`
	BearerMethodsSupported []string `json:"bearer_methods_supported"`
}

func (a *Access) QueryOAuthEnabled() bool { return a.queryOAuth != nil }

func (a *Access) QueryResource(project string) (QueryResourceMetadata, string, bool) {
	if !a.QueryOAuthEnabled() || !projectSlug.MatchString(project) {
		return QueryResourceMetadata{}, "", false
	}
	path := "/api/v1/projects/" + project + "/mcp"
	return QueryResourceMetadata{
		Resource:               a.queryOAuth.origin + path,
		AuthorizationServers:   []string{a.queryOAuth.issuer},
		ScopesSupported:        []string{QueryReadScope},
		BearerMethodsSupported: []string{"header"},
	}, a.queryOAuth.origin + ResourceMetadataPrefix + path, true
}

// CheckQueryOAuth returns the HTTP authorization status for the supplied
// resource. The caller gets the resource from QueryResource for the route.
func (a *Access) CheckQueryOAuth(r *http.Request, resource string) int {
	raw, ok := bearer(r)
	if a.queryOAuth == nil || !ok || len(r.Header.Values("Authorization")) != 1 || len(raw) > 16<<10 {
		return http.StatusUnauthorized
	}
	jws, err := jose.ParseSignedCompact(raw, accessTokenAlgorithms)
	if err != nil || len(jws.Signatures) != 1 {
		return http.StatusUnauthorized
	}
	typ, _ := jws.Signatures[0].Protected.ExtraHeaders["typ"].(string)
	if !strings.EqualFold(typ, "at+jwt") && !strings.EqualFold(typ, "application/at+jwt") {
		return http.StatusUnauthorized
	}

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	token, err := a.queryOAuth.verifier.Verify(ctx, raw)
	if err != nil || token.Issuer != a.queryOAuth.issuer || token.Subject == "" ||
		!queryAudience(token.Audience, resource, a.queryOAuth.issuer) || !token.Expiry.After(time.Now()) {
		return http.StatusUnauthorized
	}
	var claims struct {
		Scope string `json:"scope"`
	}
	if err := token.Claims(&claims); err != nil || !slices.Contains(strings.Fields(claims.Scope), QueryReadScope) {
		return http.StatusForbidden
	}
	return http.StatusOK
}

func queryAudience(audience []string, resource, issuer string) bool {
	// Pocket ID can add its own issuer for userinfo when an identity scope
	// was granted. A second tracer project or unrelated resource is rejected.
	return slices.Contains(audience, resource) &&
		(len(audience) == 1 || len(audience) == 2 && slices.Contains(audience, issuer))
}
