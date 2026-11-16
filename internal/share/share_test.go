package share

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestANSIToHTML(t *testing.T) {
	got := ANSIToHTML("\x1b[1;38;2;255;0;0mred<b>\x1b[0m & plain")
	want := `<span style="color:#ff0000;font-weight:bold">red&lt;b&gt;</span> &amp; plain`
	if got != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
}

func TestServerNeedsToken(t *testing.T) {
	s, err := Start("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Stop()
	s.Update("hello \x1b[32mworld\x1b[0m")
	base := strings.Split(s.URL, "?")[0]
	if r, err := http.Get(base + "screen?t=wrong"); err != nil || r.StatusCode != http.StatusForbidden {
		t.Fatalf("wrong token: %v %v", r, err)
	}
	r, err := http.Get(base + "screen?t=" + s.Token)
	if err != nil || r.StatusCode != 200 {
		t.Fatalf("screen: %v %v", r, err)
	}
	b, _ := io.ReadAll(r.Body)
	if !strings.Contains(string(b), "world") || s.Viewers() != 1 {
		t.Fatalf("body %q viewers %d", b, s.Viewers())
	}
}
