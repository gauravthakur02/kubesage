package serve

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"golang.org/x/net/websocket"
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
	s, err := Start("127.0.0.1:0", true, func() (tea.Model, error) { return echoModel{}, nil })
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
	s, err := StartApp("127.0.0.1:0", true, func() (tea.Model, error) { return echoModel{}, nil }, app)
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
	if c, _ := status("/api/meta", map[string]string{"X-Kubesage-Token": "wrong"}); c != 403 {
		t.Fatalf("api with wrong token = %d", c)
	}
	if c, b := status("/api/meta", map[string]string{"X-Kubesage-Token": s.Token}); c != 200 || !strings.Contains(b, "ok") {
		t.Fatalf("api with token = %d %s", c, b)
	}
	if c, b := status("/?t="+s.Token, nil); c != 200 || b != "APP "+s.Token {
		t.Fatalf("index = %d %s", c, b)
	}
	if c, b := status("/term?t="+s.Token, nil); c != 200 || !strings.Contains(b, "xterm") {
		t.Fatalf("term = %d", c)
	}
	if c, _ := status("/", nil); c != 403 {
		t.Fatalf("index without token = %d", c)
	}
}
