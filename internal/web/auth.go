package web

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
)

// User is the person signed in through OIDC.
type User struct {
	Email  string   `json:"e"`
	Groups []string `json:"g,omitempty"`
	Exp    int64    `json:"x"`
}

type userKey struct{}

// WithUser stores the signed-in user on the request.
func WithUser(r *http.Request, u *User) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), userKey{}, u))
}

// UserFrom returns the signed-in user (nil without sign-in).
func UserFrom(r *http.Request) *User {
	u, _ := r.Context().Value(userKey{}).(*User)
	return u
}

// OIDCConfig configures sign-in for kubeweave serve.
type OIDCConfig struct {
	Issuer, ClientID, ClientSecret, RedirectURL string
	AllowedDomains, AllowedEmails               []string
	GroupsClaim                                 string // default "groups"
	SessionHours                                int    // default 8
}

// OIDC signs people in with any OpenID Connect provider (Entra ID, Google, Okta, Keycloak, Dex, …).
type OIDC struct {
	cfg      OIDCConfig
	oauth    oauth2.Config
	verifier *oidc.IDTokenVerifier
	key      []byte // signs session cookies; new on every start, so a restart signs everyone out
	secure   bool
}

const (
	sessionCookie = "kubeweave_session"
	flowCookie    = "kubeweave_oidc"
)

// NewOIDC discovers the provider and prepares sign-in.
func NewOIDC(ctx context.Context, cfg OIDCConfig) (*OIDC, error) {
	if cfg.Issuer == "" || cfg.ClientID == "" || cfg.RedirectURL == "" {
		return nil, fmt.Errorf("oidc: issuer, client_id and redirect_url are required")
	}
	if !strings.HasSuffix(cfg.RedirectURL, "/auth/callback") {
		return nil, fmt.Errorf("oidc: redirect_url must end with /auth/callback")
	}
	p, err := oidc.NewProvider(ctx, cfg.Issuer)
	if err != nil {
		return nil, fmt.Errorf("oidc: %w", err)
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}
	if cfg.GroupsClaim == "" {
		cfg.GroupsClaim = "groups"
	}
	if cfg.SessionHours <= 0 {
		cfg.SessionHours = 8
	}
	return &OIDC{cfg: cfg, key: key, secure: strings.HasPrefix(cfg.RedirectURL, "https://"),
		verifier: p.Verifier(&oidc.Config{ClientID: cfg.ClientID}),
		oauth: oauth2.Config{ClientID: cfg.ClientID, ClientSecret: cfg.ClientSecret, RedirectURL: cfg.RedirectURL,
			Endpoint: p.Endpoint(), Scopes: []string{oidc.ScopeOpenID, "email", "profile"}}}, nil
}

func randomString() string {
	b := make([]byte, 24)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

func (o *OIDC) sign(v any) string {
	b, _ := json.Marshal(v)
	payload := base64.RawURLEncoding.EncodeToString(b)
	m := hmac.New(sha256.New, o.key)
	m.Write([]byte(payload))
	return payload + "." + base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

func (o *OIDC) verify(s string, v any) bool {
	payload, sig, ok := strings.Cut(s, ".")
	if !ok {
		return false
	}
	m := hmac.New(sha256.New, o.key)
	m.Write([]byte(payload))
	want := m.Sum(nil)
	got, err := base64.RawURLEncoding.DecodeString(sig)
	if err != nil || !hmac.Equal(want, got) {
		return false
	}
	b, err := base64.RawURLEncoding.DecodeString(payload)
	return err == nil && json.Unmarshal(b, v) == nil
}

func (o *OIDC) setCookie(w http.ResponseWriter, name, value, path string, age time.Duration) {
	http.SetCookie(w, &http.Cookie{Name: name, Value: value, Path: path, HttpOnly: true, Secure: o.secure,
		SameSite: http.SameSiteLaxMode, MaxAge: int(age.Seconds())})
}

// User reads the session cookie.
func (o *OIDC) User(r *http.Request) *User {
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return nil
	}
	var u User
	if !o.verify(c.Value, &u) || time.Now().Unix() > u.Exp || u.Email == "" {
		return nil
	}
	return &u
}

type flow struct {
	State, Nonce, Verifier, Back string
	Exp                          int64
}

// Login starts sign-in (/auth/login).
func (o *OIDC) Login(w http.ResponseWriter, r *http.Request) {
	f := flow{State: randomString(), Nonce: randomString(), Verifier: oauth2.GenerateVerifier(), Exp: time.Now().Add(10 * time.Minute).Unix()}
	if b := r.URL.Query().Get("back"); strings.HasPrefix(b, "/") && !strings.HasPrefix(b, "//") && !strings.ContainsAny(b, "\\\r\n") {
		f.Back = b
	}
	o.setCookie(w, flowCookie, o.sign(f), "/auth/", 10*time.Minute)
	http.Redirect(w, r, o.oauth.AuthCodeURL(f.State, oidc.Nonce(f.Nonce), oauth2.S256ChallengeOption(f.Verifier)), http.StatusFound)
}

// Callback finishes sign-in (/auth/callback).
func (o *OIDC) Callback(w http.ResponseWriter, r *http.Request) {
	c, err := r.Cookie(flowCookie)
	var f flow
	if err != nil || !o.verify(c.Value, &f) || time.Now().Unix() > f.Exp || r.URL.Query().Get("state") != f.State {
		http.Error(w, "sign-in expired or invalid; start again at /auth/login", http.StatusBadRequest)
		return
	}
	o.setCookie(w, flowCookie, "", "/auth/", -time.Second)
	if e := r.URL.Query().Get("error"); e != "" {
		http.Error(w, "sign-in failed: "+e, http.StatusForbidden)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	tok, err := o.oauth.Exchange(ctx, r.URL.Query().Get("code"), oauth2.VerifierOption(f.Verifier))
	if err != nil {
		http.Error(w, "sign-in failed: "+err.Error(), http.StatusForbidden)
		return
	}
	raw, _ := tok.Extra("id_token").(string)
	idt, err := o.verifier.Verify(ctx, raw)
	if err != nil || idt.Nonce != f.Nonce {
		http.Error(w, "sign-in failed: invalid ID token", http.StatusForbidden)
		return
	}
	var claims map[string]any
	_ = idt.Claims(&claims)
	u, err := o.userFromClaims(claims)
	if err != nil {
		http.Error(w, err.Error(), http.StatusForbidden)
		return
	}
	u.Exp = time.Now().Add(time.Duration(o.cfg.SessionHours) * time.Hour).Unix()
	o.setCookie(w, sessionCookie, o.sign(u), "/", time.Duration(o.cfg.SessionHours)*time.Hour)
	back := f.Back
	if back == "" {
		back = "/"
	}
	http.Redirect(w, r, back, http.StatusFound)
}

// userFromClaims checks who signed in against the allowed domains and emails.
func (o *OIDC) userFromClaims(claims map[string]any) (*User, error) {
	email, _ := claims["email"].(string)
	if email == "" {
		email, _ = claims["preferred_username"].(string) // Entra ID without the email claim
	}
	email = strings.ToLower(email)
	if email == "" {
		return nil, fmt.Errorf("sign-in failed: the identity provider sent no email")
	}
	if v, ok := claims["email_verified"].(bool); ok && !v {
		return nil, fmt.Errorf("sign-in failed: %s is not verified", email)
	}
	allowed := len(o.cfg.AllowedDomains) == 0 && len(o.cfg.AllowedEmails) == 0
	for _, d := range o.cfg.AllowedDomains {
		if strings.HasSuffix(email, "@"+strings.ToLower(strings.TrimPrefix(d, "@"))) {
			allowed = true
		}
	}
	for _, e := range o.cfg.AllowedEmails {
		if strings.EqualFold(e, email) {
			allowed = true
		}
	}
	if !allowed {
		return nil, fmt.Errorf("%s is not allowed to use this kubeweave", email)
	}
	u := &User{Email: email}
	if gs, ok := claims[o.cfg.GroupsClaim].([]any); ok {
		for _, g := range gs {
			if s, ok := g.(string); ok {
				u.Groups = append(u.Groups, s)
			}
		}
	}
	return u, nil
}

// Logout ends the session (POST /auth/logout; a GET cannot sign anyone out, so other sites cannot either).
func (o *OIDC) Logout(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "use POST", http.StatusMethodNotAllowed)
		return
	}
	o.setCookie(w, sessionCookie, "", "/", -time.Second)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte("Signed out of kubeweave. <a href=\"/\">Sign in again</a>"))
}
