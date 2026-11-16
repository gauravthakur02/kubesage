// Package share serves a read-only, auto-refreshing web view of what kubesage shows (:share).
package share

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"html"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Server holds the latest screen and serves it to viewers who know the token.
type Server struct {
	Token string
	URL   string

	mu     sync.Mutex
	screen string
	at     time.Time
	srv    *http.Server
	ln     net.Listener
	views  int
}

// Start listens on addr (e.g. 127.0.0.1:0) and serves the screen at /?t=<token>.
func Start(addr string) (*Server, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return nil, err
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	s := &Server{Token: hex.EncodeToString(b), ln: ln}
	host, port, _ := net.SplitHostPort(ln.Addr().String())
	if host == "::" || host == "0.0.0.0" || host == "" {
		host = localIP()
	}
	s.URL = fmt.Sprintf("http://%s/?t=%s", net.JoinHostPort(host, port), s.Token)
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.page)
	mux.HandleFunc("/screen", s.fragment)
	s.srv = &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go s.srv.Serve(ln)
	return s, nil
}

func localIP() string {
	if conn, err := net.Dial("udp", "192.0.2.1:9"); err == nil { // no packet is sent
		defer conn.Close()
		return conn.LocalAddr().(*net.UDPAddr).IP.String()
	}
	return "localhost"
}

// Update replaces the shared screen (ANSI text as rendered in the terminal).
func (s *Server) Update(screen string) {
	s.mu.Lock()
	s.screen, s.at = screen, time.Now()
	s.mu.Unlock()
}

// Viewers is how many screen fetches have been served (a rough sign someone is watching).
func (s *Server) Viewers() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.views
}

// Stop closes the server.
func (s *Server) Stop() error { return s.srv.Close() }

func (s *Server) authorized(r *http.Request) bool {
	return subtle.ConstantTimeCompare([]byte(r.URL.Query().Get("t")), []byte(s.Token)) == 1
}

func (s *Server) fragment(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(r) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	s.mu.Lock()
	screen := s.screen
	s.views++
	s.mu.Unlock()
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	fmt.Fprint(w, ANSIToHTML(screen))
}

func (s *Server) page(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" || !s.authorized(r) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	fmt.Fprintf(w, pageHTML, html.EscapeString(s.Token))
}

const pageHTML = `<!doctype html><html><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>kubesage · shared view</title>
<style>body{margin:0;background:#1a1b26;color:#c0caf5}header{font:13px system-ui,sans-serif;padding:6px 12px;color:#7aa2f7;border-bottom:1px solid #2f334d}
pre{margin:0;padding:8px 12px;font:13px/1.25 ui-monospace,Menlo,Consolas,monospace;white-space:pre;overflow:auto}</style></head>
<body><header>kubesage · read-only shared view · refreshes every second</header><pre id="s">connecting…</pre>
<script>const t=%q;async function f(){try{const r=await fetch('/screen?t='+encodeURIComponent(t),{cache:'no-store'});if(r.ok)document.getElementById('s').innerHTML=await r.text()}catch(e){}setTimeout(f,1000)}f()</script></body></html>`

// ANSIToHTML converts SGR colour/bold sequences to <span> tags and escapes everything else.
func ANSIToHTML(s string) string {
	var b strings.Builder
	open := false
	var fg, bg string
	var bold, faint, italic, underline bool
	flush := func() {
		if open {
			b.WriteString("</span>")
			open = false
		}
		var st []string
		if fg != "" {
			st = append(st, "color:"+fg)
		}
		if bg != "" {
			st = append(st, "background:"+bg)
		}
		if bold {
			st = append(st, "font-weight:bold")
		}
		if faint {
			st = append(st, "opacity:.7")
		}
		if italic {
			st = append(st, "font-style:italic")
		}
		if underline {
			st = append(st, "text-decoration:underline")
		}
		if len(st) > 0 {
			b.WriteString(`<span style="` + strings.Join(st, ";") + `">`)
			open = true
		}
	}
	for i := 0; i < len(s); {
		if s[i] == 0x1b && i+1 < len(s) && s[i+1] == '[' {
			j := i + 2
			for j < len(s) && (s[j] < 0x40 || s[j] > 0x7e) {
				j++
			}
			if j >= len(s) {
				break
			}
			if s[j] == 'm' {
				codes := strings.Split(s[i+2:j], ";")
				for k := 0; k < len(codes); k++ {
					n, _ := strconv.Atoi(codes[k])
					switch {
					case codes[k] == "" || n == 0:
						fg, bg, bold, faint, italic, underline = "", "", false, false, false, false
					case n == 1:
						bold = true
					case n == 2:
						faint = true
					case n == 3:
						italic = true
					case n == 4:
						underline = true
					case n == 22:
						bold, faint = false, false
					case n == 23:
						italic = false
					case n == 24:
						underline = false
					case n == 39:
						fg = ""
					case n == 49:
						bg = ""
					case (n == 38 || n == 48) && k+4 < len(codes) && codes[k+1] == "2":
						r, _ := strconv.Atoi(codes[k+2])
						g, _ := strconv.Atoi(codes[k+3])
						bl, _ := strconv.Atoi(codes[k+4])
						col := fmt.Sprintf("#%02x%02x%02x", r, g, bl)
						if n == 38 {
							fg = col
						} else {
							bg = col
						}
						k += 4
					case (n == 38 || n == 48) && k+2 < len(codes) && codes[k+1] == "5":
						v, _ := strconv.Atoi(codes[k+2])
						col := xterm256(v)
						if n == 38 {
							fg = col
						} else {
							bg = col
						}
						k += 2
					case n >= 30 && n <= 37, n >= 90 && n <= 97:
						fg = basic(n)
					case n >= 40 && n <= 47, n >= 100 && n <= 107:
						bg = basic(n - 10)
					}
				}
				flush()
			}
			i = j + 1
			continue
		}
		r := s[i]
		switch r {
		case '<':
			b.WriteString("&lt;")
		case '>':
			b.WriteString("&gt;")
		case '&':
			b.WriteString("&amp;")
		default:
			b.WriteByte(r)
		}
		i++
	}
	if open {
		b.WriteString("</span>")
	}
	return b.String()
}

var basicColors = []string{"#15161e", "#f7768e", "#9ece6a", "#e0af68", "#7aa2f7", "#bb9af7", "#7dcfff", "#a9b1d6"}

func basic(n int) string {
	if n >= 90 {
		return basicColors[(n-90)%8]
	}
	return basicColors[(n-30)%8]
}

func xterm256(v int) string {
	switch {
	case v < 8:
		return basicColors[v]
	case v < 16:
		return basicColors[v-8]
	case v < 232:
		v -= 16
		lv := []int{0, 95, 135, 175, 215, 255}
		return fmt.Sprintf("#%02x%02x%02x", lv[v/36], lv[(v/6)%6], lv[v%6])
	default:
		g := 8 + (v-232)*10
		return fmt.Sprintf("#%02x%02x%02x", g, g, g)
	}
}
