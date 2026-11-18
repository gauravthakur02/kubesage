package serve

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"golang.org/x/net/websocket"

	"github.com/gauravthakur02/kubeweave/internal/web"
)

// echoModel shows the last key and the window size.
type echoModel struct {
	last string
	w, h int
}

func (m echoModel) Init() tea.Cmd { return nil }
func (m echoModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch v := msg.(type) {
	case tea.KeyMsg:
		if v.String() == "q" {
			return m, tea.Quit
		}
		m.last += v.String()
	case tea.WindowSizeMsg:
		m.w, m.h = v.Width, v.Height
	}
	return m, nil
}
func (m echoModel) View() string { return "keys=" + m.last + " size=" + itoa(m.w) + "x" + itoa(m.h) }

func itoa(n int) string { b, _ := json.Marshal(n); return string(b) }

func TestServeSession(t *testing.T) {
	s, err := Start("127.0.0.1:0", true, func(*http.Request) (tea.Model, error) { return echoModel{}, nil })
	if err != nil {
		t.Fatal(err)
	}
	defer s.srv.Close()
	base := strings.TrimPrefix(strings.Split(s.URL, "/?")[0], "http://")
	if _, err := websocket.Dial("ws://"+base+"/ws?t=wrong", "", "http://"+base); err == nil {
		t.Fatal("wrong token accepted")
	}
	if _, err := websocket.Dial("ws://"+base+"/ws?t="+s.Token, "", "http://evil.example"); err == nil {
		t.Fatal("cross-origin connection accepted")
	}
	ws, err := websocket.Dial("ws://"+base+"/ws?t="+s.Token, "", "http://"+base)
	if err != nil {
		t.Fatal(err)
	}
	defer ws.Close()
	send := func(v any) { b, _ := json.Marshal(v); _ = websocket.Message.Send(ws, string(b)) }
	send(map[string]any{"t": "resize", "c": 100, "r": 30})
	send(map[string]any{"t": "in", "d": "ab"})
	deadline := time.Now().Add(5 * time.Second)
	var screen strings.Builder
	for time.Now().Before(deadline) && !strings.Contains(screen.String(), "keys=ab size=100x30") {
		_ = ws.SetReadDeadline(time.Now().Add(time.Second))
		var b []byte
		if websocket.Message.Receive(ws, &b) == nil {
			screen.Write(b)
		}
	}
	if !strings.Contains(screen.String(), "keys=ab size=100x30") {
		t.Fatalf("screen never showed the keys: %q", screen.String())
	}
	if s.Sessions() != 1 {
		t.Fatalf("sessions = %d", s.Sessions())
	}
}

func TestStartAppRoutes(t *testing.T) {
	app := &WebApp{
		API:    http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(`{"ok":true}`)) }),
		Assets: http.StripPrefix("/static/", http.FileServer(http.Dir(t.TempDir()))),
		Index:  func(tok string) []byte { return []byte("APP " + tok) },
	}
	s, err := StartApp("127.0.0.1:0", true, func(*http.Request) (tea.Model, error) { return echoModel{}, nil }, app)
	if err != nil {
		t.Fatal(err)
	}
	defer s.srv.Close()
	base := strings.Split(s.URL, "/?")[0]
	status := func(path string, hdr map[string]string) (int, string) {
		req, _ := http.NewRequest("GET", base+path, nil)
		for k, v := range hdr {
			req.Header.Set(k, v)
		}
		r, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer r.Body.Close()
		b, _ := io.ReadAll(r.Body)
		return r.StatusCode, string(b)
	}
	if c, _ := status("/api/meta", nil); c != 403 {
		t.Fatalf("api without token = %d", c)
	}
	if c, _ := status("/api/meta", map[string]string{"X-Kubeweave-Token": "wrong"}); c != 403 {
		t.Fatalf("api with wrong token = %d", c)
	}
	if c, b := status("/api/meta", map[string]string{"X-Kubeweave-Token": s.Token}); c != 200 || !strings.Contains(b, "ok") {
		t.Fatalf("api with token = %d %s", c, b)
	}
	// the link's token becomes an HttpOnly cookie and leaves the URL
	noRedirect := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	r, err := noRedirect.Get(base + "/?t=" + s.Token)
	if err != nil || r.StatusCode != 302 || r.Header.Get("Location") != "/" {
		t.Fatalf("token swap = %v %v", r, err)
	}
	var tok *http.Cookie
	for _, c := range r.Cookies() {
		if c.Name == tokenCookie {
			tok = c
		}
	}
	if tok == nil || !tok.HttpOnly || tok.SameSite != http.SameSiteStrictMode {
		t.Fatalf("cookie = %+v", tok)
	}
	withCookie := func(path string) (int, string, http.Header) {
		req, _ := http.NewRequest("GET", base+path, nil)
		req.AddCookie(tok)
		r, err := noRedirect.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer r.Body.Close()
		b, _ := io.ReadAll(r.Body)
		return r.StatusCode, string(b), r.Header
	}
	if c, b, h := withCookie("/"); c != 200 || b != "APP " || strings.Contains(h.Get("Content-Security-Policy"), "unsafe-inline") {
		t.Fatalf("index with cookie = %d %q csp=%q", c, b, h.Get("Content-Security-Policy"))
	}
	if c, b, _ := withCookie("/api/meta"); c != 200 || !strings.Contains(b, "ok") {
		t.Fatalf("api with cookie = %d", c)
	}
	if c, b, _ := withCookie("/term"); c != 200 || !strings.Contains(b, `integrity="sha384-`) || strings.Contains(b, s.Token) {
		t.Fatalf("term with cookie = %d (SRI or token leak)", c)
	}
	if r, _ := noRedirect.Get(base + "/term?t=" + s.Token); r.StatusCode != 302 || r.Header.Get("Location") != "/term" {
		t.Fatalf("term with token = %d %s", r.StatusCode, r.Header.Get("Location"))
	}
	if c, _ := status("/", nil); c != 403 {
		t.Fatalf("index without token = %d", c)
	}
}

func TestSignInRequired(t *testing.T) {
	var idp *httptest.Server
	idp = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"issuer": idp.URL, "authorization_endpoint": idp.URL + "/authorize",
			"token_endpoint": idp.URL + "/token", "jwks_uri": idp.URL + "/jwks", "id_token_signing_alg_values_supported": []string{"RS256"}})
	}))
	defer idp.Close()
	auth, err := web.NewOIDC(context.Background(), web.OIDCConfig{Issuer: idp.URL, ClientID: "kubeweave", RedirectURL: "http://127.0.0.1/auth/callback"})
	if err != nil {
		t.Fatal(err)
	}
	app := &WebApp{API: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("api")) }),
		Assets: http.NotFoundHandler(), Index: func(string) []byte { return []byte("APP") }, Auth: auth, PublicURL: "http://127.0.0.1/"}
	s, err := StartApp("127.0.0.1:0", true, func(*http.Request) (tea.Model, error) { return echoModel{}, nil }, app)
	if err != nil {
		t.Fatal(err)
	}
	defer s.srv.Close()
	base := "http://" + s.ln.Addr().String()
	cl := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	for path, want := range map[string]int{"/": 302, "/term": 302, "/api/meta": 403, "/?t=" + s.Token: 302, "/auth/login": 302} {
		r, err := cl.Get(base + path)
		if err != nil {
			t.Fatal(err)
		}
		r.Body.Close()
		if r.StatusCode != want {
			t.Errorf("%s = %d, want %d", path, r.StatusCode, want)
		}
	}
	req, _ := http.NewRequest("GET", base+"/api/meta", nil)
	req.Header.Set("X-Kubeweave-Token", s.Token)
	if r, _ := cl.Do(req); r.StatusCode != 403 {
		t.Fatalf("token accepted although sign-in is on: %d", r.StatusCode)
	}
	if s.URL != "http://127.0.0.1/" {
		t.Fatalf("printed URL = %s", s.URL)
	}
}
