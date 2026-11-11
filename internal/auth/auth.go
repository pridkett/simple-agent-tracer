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
	"slices"
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
	Issuer, Client, Secret, Callback, AdminGroup string
	LegacyOwnerSubject                           string
}

type Access struct {
	local         bool
	credentials   []credential
	session       *scs.SessionManager
	provider      *oidc.IDTokenVerifier
	oauth         *oauth2.Config
	adminGroup    string
	legacySubject string
	origin        string
}

var namespace = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,127}$`)

func New(ctx context.Context, local bool, raw string, config OIDCConfig) (*Access, []Credential, error) {
	if local && config.Issuer != "" {
		return nil, nil, errors.New("local mode cannot be used with OIDC")
	}
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
			redirect.RawQuery != "" || redirect.Fragment != "" || config.Client == "" {
			return nil, nil, errors.New("OIDC requires a client and HTTPS /auth/callback URL")
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
		a.oauth = &oauth2.Config{
			ClientID: config.Client, ClientSecret: config.Secret, RedirectURL: config.Callback,
			Endpoint: provider.Endpoint(), Scopes: []string{oidc.ScopeOpenID, "email", "profile", "groups"},
		}
		a.adminGroup = config.AdminGroup
		if a.adminGroup == "" {
			a.adminGroup = "admin"
			// Keep an existing subject-only deployment restricted until its administrator opts into group access.
			a.legacySubject = config.LegacyOwnerSubject
		}
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
	if a.local {
		return "", a.localRequest(r)
	}
	return "", a.session != nil && a.session.GetBool(r.Context(), "owner") &&
		(!mutate || r.Header.Get("Origin") == a.origin)
}

func (a *Access) localRequest(r *http.Request) bool {
	peer, err := netip.ParseAddrPort(r.RemoteAddr)
	if !a.local || err != nil || !peer.Addr().Unmap().IsLoopback() || r.Header.Get("Sec-Fetch-Site") == "cross-site" {
		return false
	}
	host := r.Host
	if value, _, err := net.SplitHostPort(host); err == nil {
		host = value
	}
	host = strings.Trim(host, "[]")
	ip, _ := netip.ParseAddr(host)
	if !ip.Unmap().IsLoopback() && !strings.EqualFold(host, "localhost") {
		return false
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	origin := r.Header.Get("Origin")
	return origin == "" || strings.EqualFold(origin, scheme+"://"+r.Host)
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
		"local":    a.localRequest(r),
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
	if err != nil || id.Subject == "" || id.Nonce != nonce {
		http.Error(w, "invalid ID token", http.StatusForbidden)
		return
	}
	var claims struct {
		Groups        []string `json:"groups"`
		EmailVerified *bool    `json:"email_verified"`
	}
	if err = id.Claims(&claims); err != nil {
		http.Error(w, "invalid ID token claims", http.StatusForbidden)
		return
	}
	if claims.EmailVerified != nil && !*claims.EmailVerified {
		http.Error(w, "email not verified", http.StatusForbidden)
		return
	}
	authorized := slices.Contains(claims.Groups, a.adminGroup)
	if a.legacySubject != "" {
		authorized = id.Subject == a.legacySubject
	}
	if !authorized {
		http.Error(w, "administrator login required", http.StatusForbidden)
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
