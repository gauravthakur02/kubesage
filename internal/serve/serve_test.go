package serve

import (
	"encoding/json"
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
