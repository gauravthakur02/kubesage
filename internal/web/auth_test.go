package web

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
)

// fakeIdP is a minimal OpenID Connect provider that signs ID tokens with its own key.
type fakeIdP struct {
	srv   *httptest.Server
	key   *rsa.PrivateKey
	nonce string
	email string
}

func newIdP(t *testing.T) *fakeIdP {
	k, _ := rsa.GenerateKey(rand.Reader, 2048)
	p := &fakeIdP{key: k, email: "alice@example.com"}
	mux := http.NewServeMux()
	p.srv = httptest.NewServer(mux)
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"issuer": p.srv.URL, "authorization_endpoint": p.srv.URL + "/authorize",
			"token_endpoint": p.srv.URL + "/token", "jwks_uri": p.srv.URL + "/jwks", "id_token_signing_alg_values_supported": []string{"RS256"}})
	})
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: &k.PublicKey, KeyID: "k1", Algorithm: "RS256", Use: "sig"}}})
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if r.Form.Get("code") != "good-code" || r.Form.Get("code_verifier") == "" {
			http.Error(w, `{"error":"invalid_grant"}`, 400)
			return
		}
		sig, _ := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: k}, (&jose.SignerOptions{}).WithHeader("kid", "k1"))
		tok, _ := jwt.Signed(sig).Claims(map[string]any{"iss": p.srv.URL, "aud": "kubesage", "sub": "1", "email": p.email, "email_verified": true,
			"groups": []string{"sre"}, "nonce": p.nonce, "iat": time.Now().Unix(), "exp": time.Now().Add(time.Hour).Unix()}).Serialize()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "at", "token_type": "Bearer", "expires_in": 3600, "id_token": tok})
	})
	t.Cleanup(p.srv.Close)
	return p
}

func TestOIDCSignIn(t *testing.T) {
	idp := newIdP(t)
	app := httptest.NewServer(nil)
	defer app.Close()
	o, err := NewOIDC(context.Background(), OIDCConfig{Issuer: idp.srv.URL, ClientID: "kubesage", RedirectURL: app.URL + "/auth/callback", AllowedDomains: []string{"example.com"}})
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/auth/login", o.Login)
	mux.HandleFunc("/auth/callback", o.Callback)
	mux.HandleFunc("/me", func(w http.ResponseWriter, r *http.Request) {
		if u := o.User(r); u != nil {
			_, _ = w.Write([]byte(u.Email + " " + strings.Join(u.Groups, ",")))
			return
		}
		http.Error(w, "nobody", 401)
	})
	app.Config.Handler = mux
	jar, _ := cookiejar.New(nil)
	cl := &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	signIn := func() (*http.Response, error) {
		r, err := cl.Get(app.URL + "/auth/login?back=/me")
		if err != nil {
			return nil, err
		}
		loc, _ := url.Parse(r.Header.Get("Location"))
		if loc.Query().Get("code_challenge") == "" {
			t.Fatal("no PKCE challenge")
		}
		idp.nonce = loc.Query().Get("nonce")
		return cl.Get(app.URL + "/auth/callback?code=good-code&state=" + url.QueryEscape(loc.Query().Get("state")))
	}
	r, err := signIn()
	if err != nil || r.StatusCode != http.StatusFound || r.Header.Get("Location") != "/me" {
		t.Fatalf("callback = %v %v", r, err)
	}
	me, _ := cl.Get(app.URL + "/me")
	b := make([]byte, 100)
	n, _ := me.Body.Read(b)
	if me.StatusCode != 200 || string(b[:n]) != "alice@example.com sre" {
		t.Fatalf("me = %d %q", me.StatusCode, b[:n])
	}
	// a forged session cookie is refused
	u, _ := url.Parse(app.URL)
	jar.SetCookies(u, []*http.Cookie{{Name: sessionCookie, Value: "eyJlIjoiYm9iQGV4YW1wbGUuY29tIiwieCI6OTk5OTk5OTk5OX0.bad"}})
	if me, _ := cl.Get(app.URL + "/me"); me.StatusCode != 401 {
		t.Fatalf("forged cookie accepted: %d", me.StatusCode)
	}
	// someone outside the allowed domains
	idp.email = "mallory@evil.example"
	if r, _ := signIn(); r.StatusCode != http.StatusForbidden {
		t.Fatalf("outsider signed in: %d", r.StatusCode)
	}
	// a callback without the flow cookie (or with a wrong state) fails
	cl2 := &http.Client{CheckRedirect: cl.CheckRedirect}
	if r, _ := cl2.Get(app.URL + "/auth/callback?code=good-code&state=x"); r.StatusCode != http.StatusBadRequest {
		t.Fatalf("callback without flow = %d", r.StatusCode)
	}
}
