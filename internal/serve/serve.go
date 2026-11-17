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
)

// NewModel builds a fresh UI for one browser session.
type NewModel func() (tea.Model, error)

// Server serves the web UI.
type Server struct {
	Token    string
	URL      string
	ReadOnly bool
	srv      *http.Server
	ln       net.Listener
	mu       sync.Mutex
	sessions int
}

// WebApp is the browser UI mounted at / (the terminal UI then lives at /term).
type WebApp struct {
	API    http.Handler        // /api/*, only reached with the session token
	Assets http.Handler        // /static/* (public files, no cluster data)
	Index  func(string) []byte // the app page with the token filled in
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
	s := &Server{Token: hex.EncodeToString(b), ln: ln, ReadOnly: readOnly}
	host, port, _ := net.SplitHostPort(ln.Addr().String())
	if host == "::" || host == "0.0.0.0" || host == "" {
		host = outboundIP()
	}
	s.URL = fmt.Sprintf("http://%s/?t=%s", net.JoinHostPort(host, port), s.Token)
	mux := http.NewServeMux()
	if app != nil {
		mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/" || !s.ok(r) {
				http.Error(w, "forbidden: open the link printed by kubesage serve", http.StatusForbidden)
				return
			}
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Header().Set("Cache-Control", "no-store")
			w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self' 'unsafe-inline'; style-src 'self'; img-src 'self' data:; connect-src 'self'; frame-ancestors 'none'")
			w.Header().Set("Referrer-Policy", "no-referrer")
			_, _ = w.Write(app.Index(s.Token))
		})
		mux.Handle("/static/", app.Assets)
		mux.Handle("/api/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if subtle.ConstantTimeCompare([]byte(r.Header.Get("X-Kubesage-Token")), []byte(s.Token)) != 1 && !s.ok(r) {
				http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
				return
			}
			app.API.ServeHTTP(w, r)
		}))
		mux.HandleFunc("/term", func(w http.ResponseWriter, r *http.Request) { s.page(w, r) })
	} else {
		mux.HandleFunc("/", s.page)
	}
	mux.Handle("/ws", websocket.Server{Handler: func(ws *websocket.Conn) { s.session(ws, newModel) }, Handshake: s.handshake})
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
	if !s.ok(r) {
		return fmt.Errorf("forbidden")
	}
	// Same-origin only: a page on another site must not drive this session.
	if o := r.Header.Get("Origin"); o != "" && !strings.EqualFold(strings.TrimPrefix(strings.TrimPrefix(o, "http://"), "https://"), r.Host) {
		return fmt.Errorf("cross-origin request refused")
	}
	return nil
}

func (s *Server) page(w http.ResponseWriter, r *http.Request) {
	if (r.URL.Path != "/" && r.URL.Path != "/term") || !s.ok(r) {
		http.Error(w, "forbidden: open the link printed by kubesage serve", http.StatusForbidden)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self' 'unsafe-inline' https://cdn.jsdelivr.net; style-src 'self' 'unsafe-inline' https://cdn.jsdelivr.net; connect-src 'self'")
	mode := "read-only"
	if !s.ReadOnly {
		mode = "changes allowed"
	}
	fmt.Fprint(w, strings.NewReplacer("{{TOKEN}}", s.Token, "{{MODE}}", mode).Replace(pageHTML))
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

func (s *Server) session(ws *websocket.Conn, newModel NewModel) {
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
<link rel="stylesheet" href="https://cdn.jsdelivr.net/npm/@xterm/xterm@5.5.0/css/xterm.css">
<style>html,body{margin:0;height:100%;background:#1a1b26}#bar{font:12px system-ui,sans-serif;color:#7aa2f7;padding:4px 10px;border-bottom:1px solid #2f334d}
#t{position:absolute;top:24px;left:0;right:0;bottom:0;padding:4px}</style></head>
<body><div id="bar">kubesage · {{MODE}} · this tab is its own session</div><div id="t"></div>
<script src="https://cdn.jsdelivr.net/npm/@xterm/xterm@5.5.0/lib/xterm.js"></script>
<script src="https://cdn.jsdelivr.net/npm/@xterm/addon-fit@0.10.0/lib/addon-fit.js"></script>
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
