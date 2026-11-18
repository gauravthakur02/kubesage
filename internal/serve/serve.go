// Package serve runs the kubesage UI in a web browser: each browser tab gets its own Bubble Tea program whose
// terminal input and output travel over a WebSocket to xterm.js. No pseudo-terminal is needed, so it works on
// every OS.
package serve

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"golang.org/x/net/websocket"

	"github.com/gauravthakur02/kubesage/internal/web"
)

// NewModel builds a fresh UI for one browser session; r carries the signed-in user (web.UserFrom), if any.
type NewModel func(r *http.Request) (tea.Model, error)

// Server serves the web UI.
type Server struct {
	Token    string
	URL      string
	ReadOnly bool
	app      *WebApp
	srv      *http.Server
	ln       net.Listener
	mu       sync.Mutex
	sessions int
}

// WebApp is the browser UI mounted at / (the terminal UI then lives at /term).
type WebApp struct {
	API    http.Handler        // /api/*, only reached when signed in or with the session token
	Assets http.Handler        // /static/* (public files, no cluster data)
	Index  func(string) []byte // the app page
	// Auth signs people in with OIDC; without it the random token in the link is the only key.
	Auth *web.OIDC
	// PublicURL is the address people open when Auth is on (from the OIDC redirect URL).
	PublicURL string
}

// authorize says whether a request may use the app, and attaches the signed-in user.
func (s *Server) authorize(r *http.Request) (*http.Request, bool) {
	if s.app != nil && s.app.Auth != nil {
		u := s.app.Auth.User(r)
		if u == nil {
			return r, false
		}
		return web.WithUser(r, u), true
	}
	if s.ok(r) || subtle.ConstantTimeCompare([]byte(r.Header.Get("X-Kubesage-Token")), []byte(s.Token)) == 1 {
		return r, true
	}
	if c, err := r.Cookie(tokenCookie); err == nil && subtle.ConstantTimeCompare([]byte(c.Value), []byte(s.Token)) == 1 {
		return r, true
	}
	return r, false
}

// tokenCookie carries the link's token after the first visit, so the token leaves the address bar, history and
// Referer headers. HttpOnly and SameSite=Strict: page scripts cannot read it and other sites cannot send it.
const tokenCookie = "kubesage_token"

// swapTokenForCookie turns ?t=<token> into a cookie and redirects to the same page without the token.
func (s *Server) swapTokenForCookie(w http.ResponseWriter, r *http.Request) bool {
	if s.app != nil && s.app.Auth != nil || !s.ok(r) {
		return false
	}
	http.SetCookie(w, &http.Cookie{Name: tokenCookie, Value: s.Token, Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode, Secure: r.TLS != nil})
	q := r.URL.Query()
	q.Del("t")
	u := *r.URL
	u.RawQuery = q.Encode()
	http.Redirect(w, r, u.RequestURI(), http.StatusFound)
	return true
}

func securityHeaders(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("X-Content-Type-Options", "nosniff")
}

// Start listens on addr and serves sessions created by newModel. Only requests carrying the token are served.
func Start(addr string, readOnly bool, newModel NewModel) (*Server, error) {
	return StartApp(addr, readOnly, newModel, nil)
}

// StartApp is Start with the web app at / and the terminal UI at /term.
func StartApp(addr string, readOnly bool, newModel NewModel, app *WebApp) (*Server, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return nil, err
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	s := &Server{Token: hex.EncodeToString(b), ln: ln, ReadOnly: readOnly, app: app}
	host, port, _ := net.SplitHostPort(ln.Addr().String())
	if host == "::" || host == "0.0.0.0" || host == "" {
		host = outboundIP()
	}
	s.URL = fmt.Sprintf("http://%s/?t=%s", net.JoinHostPort(host, port), s.Token)
	if app != nil && app.Auth != nil {
		s.URL = app.PublicURL
	}
	mux := http.NewServeMux()
	if app != nil {
		if app.Auth != nil {
			mux.HandleFunc("/auth/login", app.Auth.Login)
			mux.HandleFunc("/auth/callback", app.Auth.Callback)
			mux.HandleFunc("/auth/logout", app.Auth.Logout)
		}
		mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/" {
				http.NotFound(w, r)
				return
			}
			if s.swapTokenForCookie(w, r) {
				return
			}
			if _, ok := s.authorize(r); !ok {
				if app.Auth != nil {
					http.Redirect(w, r, "/auth/login?back=/", http.StatusFound)
					return
				}
				http.Error(w, "forbidden: open the link printed by kubesage serve", http.StatusForbidden)
				return
			}
			securityHeaders(w)
			_, _ = w.Write(app.Index("")) // the cookie authenticates; no token in the page
		})
		mux.Handle("/static/", app.Assets)
		mux.Handle("/api/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			r2, ok := s.authorize(r)
			if !ok {
				w.Header().Set("Content-Type", "application/json")
				http.Error(w, `{"error":"forbidden: sign in again"}`, http.StatusForbidden)
				return
			}
			if r.Method != http.MethodGet && r.Header.Get("X-Kubesage-Token") == "" && r.Header.Get("X-Kubesage") == "" {
				http.Error(w, `{"error":"missing X-Kubesage header"}`, http.StatusForbidden) // blocks cross-site form posts
				return
			}
			app.API.ServeHTTP(w, r2)
		}))
		mux.HandleFunc("/term", func(w http.ResponseWriter, r *http.Request) { s.page(w, r) })
	} else {
		mux.HandleFunc("/", s.page)
	}
	mux.Handle("/ws", websocket.Server{Handler: func(ws *websocket.Conn) {
		r, _ := s.authorize(ws.Request())
		s.session(ws, func() (tea.Model, error) { return newModel(r) })
	}, Handshake: s.handshake})
	s.srv = &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go s.srv.Serve(ln)
	return s, nil
}

func outboundIP() string {
	if c, err := net.Dial("udp", "192.0.2.1:9"); err == nil {
		defer c.Close()
		return c.LocalAddr().(*net.UDPAddr).IP.String()
	}
	return "localhost"
}

// Sessions is the number of open browser sessions.
func (s *Server) Sessions() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sessions
}

// Wait blocks until ctx ends, then shuts the server down.
func (s *Server) Wait(ctx context.Context) error {
	<-ctx.Done()
	sc, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	return s.srv.Shutdown(sc)
}

func (s *Server) ok(r *http.Request) bool {
	return subtle.ConstantTimeCompare([]byte(r.URL.Query().Get("t")), []byte(s.Token)) == 1
}

func (s *Server) handshake(cfg *websocket.Config, r *http.Request) error {
	if _, ok := s.authorize(r); !ok {
		return fmt.Errorf("forbidden")
	}
	// Same-origin only: a page on another site must not drive this session.
	if o := r.Header.Get("Origin"); o != "" && !strings.EqualFold(strings.TrimPrefix(strings.TrimPrefix(o, "http://"), "https://"), r.Host) {
		return fmt.Errorf("cross-origin request refused")
	}
	return nil
}

func (s *Server) page(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" && r.URL.Path != "/term" {
		http.NotFound(w, r)
		return
	}
	if s.swapTokenForCookie(w, r) {
		return
	}
	if _, ok := s.authorize(r); !ok {
		if s.app != nil && s.app.Auth != nil {
			http.Redirect(w, r, "/auth/login?back=/term", http.StatusFound)
			return
		}
		http.Error(w, "forbidden: open the link printed by kubesage serve", http.StatusForbidden)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self' 'unsafe-inline' https://cdn.jsdelivr.net; style-src 'self' 'unsafe-inline' https://cdn.jsdelivr.net; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'")
	w.Header().Set("Referrer-Policy", "no-referrer")
	mode := "read-only"
	if !s.ReadOnly {
		mode = "changes allowed"
	}
	token := s.Token
	if _, err := r.Cookie(tokenCookie); err == nil || (s.app != nil && s.app.Auth != nil) {
		token = "" // a cookie authenticates; the token is not handed out again
	}
	fmt.Fprint(w, strings.NewReplacer("{{TOKEN}}", token, "{{MODE}}", mode).Replace(pageHTML))
}

type wsWriter struct {
	mu sync.Mutex
	ws *websocket.Conn
}

func (w *wsWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if err := websocket.Message.Send(w.ws, p); err != nil {
		return 0, err
	}
	return len(p), nil
}

type clientMsg struct {
	T    string `json:"t"` // in, resize
	D    string `json:"d"`
	Cols int    `json:"c"`
	Rows int    `json:"r"`
}

func (s *Server) session(ws *websocket.Conn, newModel func() (tea.Model, error)) {
	defer ws.Close()
	ws.PayloadType = websocket.BinaryFrame
	m, err := newModel()
	if err != nil {
		_ = websocket.Message.Send(ws, []byte("\r\nkubesage: "+err.Error()+"\r\n"))
		return
	}
	s.mu.Lock()
	s.sessions++
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.sessions--
		s.mu.Unlock()
	}()
	inR, inW := io.Pipe()
	out := &wsWriter{ws: ws}
	p := tea.NewProgram(m, tea.WithInput(inR), tea.WithOutput(out), tea.WithAltScreen(), tea.WithoutSignalHandler())
	go func() {
		defer inW.Close()
		for {
			var raw string
			if err := websocket.Message.Receive(ws, &raw); err != nil {
				p.Quit()
				return
			}
			var cm clientMsg
			if json.Unmarshal([]byte(raw), &cm) != nil {
				continue
			}
			switch cm.T {
			case "in":
				// Plain text arrives in one chunk when pasted or typed fast; send it one key at a time so the UI
				// sees keystrokes (":" opens the command line) instead of one multi-character key.
				chunks := []string{cm.D}
				if !strings.Contains(cm.D, "\x1b") && len([]rune(cm.D)) > 1 && len(cm.D) < 512 {
					chunks = strings.Split(cm.D, "")
				}
				for _, ch := range chunks {
					if _, err := inW.Write([]byte(ch)); err != nil {
						return
					}
				}
			case "resize":
				if cm.Cols > 10 && cm.Rows > 5 && cm.Cols < 1000 && cm.Rows < 500 {
					p.Send(tea.WindowSizeMsg{Width: cm.Cols, Height: cm.Rows})
				}
			}
		}
	}()
	_, _ = p.Run()
	_ = websocket.Message.Send(ws, []byte("\r\n\x1b[2mkubesage session ended · reload the page to start a new one\x1b[0m\r\n"))
}

const pageHTML = `<!doctype html><html><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>kubesage</title>
<link rel="stylesheet" href="https://cdn.jsdelivr.net/npm/@xterm/xterm@5.5.0/css/xterm.css" integrity="sha384-8Xk9wy/gzEDUKrXtrmCFa2bBuK3BpjpDuL/p0SeKQX19Khl/M+lHOgD/CyYf7efP" crossorigin="anonymous">
<style>html,body{margin:0;height:100%;background:#1a1b26}#bar{font:12px system-ui,sans-serif;color:#7aa2f7;padding:4px 10px;border-bottom:1px solid #2f334d}
#t{position:absolute;top:24px;left:0;right:0;bottom:0;padding:4px}</style></head>
<body><div id="bar">kubesage · {{MODE}} · this tab is its own session</div><div id="t"></div>
<script src="https://cdn.jsdelivr.net/npm/@xterm/xterm@5.5.0/lib/xterm.js" integrity="sha384-M169f14mRZOXm3hD/v2Ti0ThIT/RnAQagXA9nlE15yHAtrW19gdePJh/HaTzUOe/" crossorigin="anonymous"></script>
<script src="https://cdn.jsdelivr.net/npm/@xterm/addon-fit@0.10.0/lib/addon-fit.js" integrity="sha384-iF+jqbuti4XlB64clWgFWYEscb+UnSRv3VgVikGYZu+otNFnSHr7y7NcKfBnGizn" crossorigin="anonymous"></script>
<script>
const term=new Terminal({fontFamily:'ui-monospace,Menlo,Consolas,monospace',fontSize:14,cursorBlink:false,allowProposedApi:true,
 theme:{background:'#1a1b26',foreground:'#c0caf5'}});
const fit=new FitAddon.FitAddon();term.loadAddon(fit);term.open(document.getElementById('t'));fit.fit();term.focus();
const proto=location.protocol==='https:'?'wss':'ws';
const ws=new WebSocket(proto+'://'+location.host+'/ws?t='+encodeURIComponent('{{TOKEN}}'));ws.binaryType='arraybuffer';
const send=o=>{if(ws.readyState===1)ws.send(JSON.stringify(o))};
const size=()=>{fit.fit();send({t:'resize',c:term.cols,r:term.rows})};
ws.onopen=size;ws.onmessage=e=>term.write(new Uint8Array(e.data));
ws.onclose=()=>term.write('\r\n\x1b[31mdisconnected\x1b[0m\r\n');
term.onData(d=>send({t:'in',d:d}));window.addEventListener('resize',size);
</script></body></html>`
