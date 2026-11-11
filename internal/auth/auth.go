package auth

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/alexedwards/scs/v2"
	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/pridkett/simple_agent_tracer/internal/ingest"
	"golang.org/x/oauth2"
)

type Credential struct {
	Token   string `json:"token"`
	Project string `json:"project"`
	Source  string `json:"source,omitempty"`
	Access  string `json:"access"`
}

type credential struct {
	hash                    [32]byte
	project, source, access string
}

type OIDCConfig struct {
	Issuer, Client, Secret, Callback, Subject string
}

type Access struct {
	local           bool
	credentials     []credential
	session         *scs.SessionManager
	provider        *oidc.IDTokenVerifier
	oauth           *oauth2.Config
	subject, origin string
}

var namespace = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,127}$`)

func New(ctx context.Context, local bool, raw string, config OIDCConfig) (*Access, []Credential, error) {
	a := &Access{local: local}
	var credentials []Credential
	if raw != "" && json.Unmarshal([]byte(raw), &credentials) != nil {
		return nil, nil, errors.New("invalid TRACER_CREDENTIALS JSON")
	}
	var read, write bool
	for i, c := range credentials {
		if len(c.Token) < 24 || strings.ContainsAny(c.Token, " \r\n\t") || !namespace.MatchString(c.Project) ||
			(c.Access != "read" && c.Access != "ingest") || (c.Access == "ingest" && !namespace.MatchString(c.Source)) || (c.Access == "read" && c.Source != "") {
			return nil, nil, errors.New("invalid credential configuration")
		}
		hash := sha256.Sum256([]byte(c.Token))
		for _, existing := range a.credentials {
			if existing.hash == hash {
				return nil, nil, errors.New("duplicate credential token")
			}
		}
		a.credentials = append(a.credentials, credential{hash, c.Project, c.Source, c.Access})
		credentials[i].Token = ""
		write = write || c.Access == "ingest"
		read = read || c.Access == "read"
	}
	if config.Issuer != "" {
		redirect, err := url.Parse(config.Callback)
		if err != nil || redirect.Scheme != "https" || redirect.Host == "" || redirect.User != nil || redirect.Path != "/auth/callback" ||
			redirect.RawQuery != "" || redirect.Fragment != "" || config.Client == "" || config.Subject == "" {
			return nil, nil, errors.New("OIDC requires a client, owner subject and HTTPS /auth/callback URL")
		}
		issuer, err := url.Parse(config.Issuer)
		if err != nil || issuer.Scheme != "https" || issuer.Host == "" || issuer.User != nil {
			return nil, nil, errors.New("OIDC requires an HTTPS issuer")
		}
		provider, err := oidc.NewProvider(ctx, config.Issuer)
		if err != nil {
			return nil, nil, errors.New("OIDC discovery failed")
		}
		a.provider = provider.Verifier(&oidc.Config{ClientID: config.Client})
		a.oauth = &oauth2.Config{ClientID: config.Client, ClientSecret: config.Secret, RedirectURL: config.Callback, Endpoint: provider.Endpoint(), Scopes: []string{oidc.ScopeOpenID}}
		a.subject = config.Subject
		a.origin = redirect.Scheme + "://" + redirect.Host
		a.session = scs.New()
		a.session.Cookie.Name = "__Host-tracer"
		a.session.Cookie.Secure = true
		a.session.Cookie.SameSite = http.SameSiteLaxMode
		a.session.Lifetime = 12 * time.Hour
		a.session.IdleTimeout = time.Hour
	}
	if !local && (!write || (!read && a.session == nil)) {
		return nil, nil, errors.New("external listener requires ingest credentials and read credentials or OIDC")
	}
	return a, credentials, nil
}

func (a *Access) token(r *http.Request, scope string) (credential, bool) {
	scheme, value, ok := strings.Cut(r.Header.Get("Authorization"), " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return credential{}, false
	}
	hash := sha256.Sum256([]byte(value))
	var match credential
	for _, c := range a.credentials {
		if subtle.ConstantTimeCompare(c.hash[:], hash[:]) == 1 && c.access == scope {
			match = c
		}
	}
	return match, match.access != ""
}

func (a *Access) Ingest(r *http.Request) (ingest.Identity, bool) {
	if r.Header.Get("Authorization") == "" && a.localRequest(r) {
		return ingest.Identity{Project: "default", Source: "local"}, true
	}
	c, ok := a.token(r, "ingest")
	return ingest.Identity{Project: c.project, Source: c.source}, ok
}

// Query returns the allowed project slug, or an empty slug for the owner.
func (a *Access) Query(r *http.Request, mutate bool) (string, bool) {
	if r.Header.Get("Authorization") != "" {
		c, ok := a.token(r, "read")
		return c.project, ok && !mutate
	}
	if !mutate {
		return "", a.localRequest(r) || (a.session != nil && a.session.GetBool(r.Context(), "owner"))
	}
	origin := r.Header.Get("Origin")
	if origin != "" && !a.validOrigin(r, origin) {
		return "", false
	}
	if a.localRequest(r) {
		return "", r.Header.Get("Sec-Fetch-Site") != "cross-site"
	}
	return "", origin == a.origin && a.session != nil && a.session.GetBool(r.Context(), "owner")
}

func (a *Access) localRequest(r *http.Request) bool {
	if !a.local {
		return false
	}
	host := r.Host
	if value, _, err := net.SplitHostPort(host); err == nil {
		host = value
	}
	host = strings.Trim(host, "[]")
	ip, _ := netip.ParseAddr(host)
	return ip.IsLoopback() || strings.EqualFold(host, "localhost")
}

func (a *Access) validOrigin(r *http.Request, origin string) bool {
	if a.origin != "" && origin == a.origin {
		return true
	}
	parsed, err := url.Parse(origin)
	return a.local && err == nil && parsed.Host == r.Host && (parsed.Scheme == "http" || parsed.Scheme == "https")
}

func (a *Access) Middleware(next http.Handler) http.Handler {
	if a.session == nil {
		return next
	}
	loaded := a.session.LoadAndSave(next)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/traces" {
			next.ServeHTTP(w, r)
			return
		}
		loaded.ServeHTTP(w, r)
	})
}

func (a *Access) Status(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]bool{
		"oidc":     a.session != nil,
		"signedIn": a.session != nil && a.session.GetBool(r.Context(), "owner"),
	})
}

func (a *Access) Login(w http.ResponseWriter, r *http.Request) {
	if a.oauth == nil {
		http.NotFound(w, r)
		return
	}
	state, nonce, verifier := oauth2.GenerateVerifier(), oauth2.GenerateVerifier(), oauth2.GenerateVerifier()
	a.session.Put(r.Context(), "state", state)
	a.session.Put(r.Context(), "nonce", nonce)
	a.session.Put(r.Context(), "verifier", verifier)
	http.Redirect(w, r, a.oauth.AuthCodeURL(state, oidc.Nonce(nonce), oauth2.S256ChallengeOption(verifier)), http.StatusSeeOther)
}

func (a *Access) Callback(w http.ResponseWriter, r *http.Request) {
	if a.oauth == nil {
		http.NotFound(w, r)
		return
	}
	ctx := r.Context()
	state := a.session.PopString(ctx, "state")
	nonce := a.session.PopString(ctx, "nonce")
	verifier := a.session.PopString(ctx, "verifier")
	if state == "" || nonce == "" || verifier == "" || subtle.ConstantTimeCompare([]byte(state), []byte(r.URL.Query().Get("state"))) != 1 {
		http.Error(w, "invalid login state", http.StatusBadRequest)
		return
	}
	token, err := a.oauth.Exchange(ctx, r.URL.Query().Get("code"), oauth2.VerifierOption(verifier))
	if err != nil {
		http.Error(w, "login exchange failed", http.StatusUnauthorized)
		return
	}
	raw, _ := token.Extra("id_token").(string)
	id, err := a.provider.Verify(ctx, raw)
	if err != nil || id.Subject != a.subject || id.Nonce != nonce {
		http.Error(w, "owner login required", http.StatusForbidden)
		return
	}
	if err = a.session.RenewToken(ctx); err != nil {
		http.Error(w, "session unavailable", http.StatusInternalServerError)
		return
	}
	a.session.Put(ctx, "owner", true)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (a *Access) Logout(w http.ResponseWriter, r *http.Request) {
	if a.session == nil {
		http.NotFound(w, r)
		return
	}
	if _, ok := a.Query(r, true); !ok {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	if err := a.session.Destroy(r.Context()); err != nil {
		http.Error(w, "session unavailable", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
