package update

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestNewer(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{{"0.1.0", "0.2.0", true}, {"0.2.0", "0.2.0", false}, {"1.0.0", "0.9.9", false}, {"v0.1.9", "0.1.10", true}, {"0.1.0-dev", "0.1.0", false}}
	for _, c := range cases {
		if got := Newer(c.a, c.b); got != c.want {
			t.Errorf("Newer(%s,%s)=%v", c.a, c.b, got)
		}
	}
	if IsRelease("0.1.0-dev") || IsRelease("v0.1.0-3-gabc") || !IsRelease("0.2.0") {
		t.Error("IsRelease")
	}
}

func TestLatest(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("User-Agent") != "kubesage-update-check" {
			t.Errorf("unexpected user agent %q", r.Header.Get("User-Agent"))
		}
		w.Write([]byte(`{"tag_name":"v0.3.1","html_url":"https://example.com/r"}`))
	}))
	defer srv.Close()
	LatestURL = srv.URL
	r, err := Latest(context.Background())
	if err != nil || r.Version != "0.3.1" {
		t.Fatalf("%v %+v", err, r)
	}
}

func TestApplyVerifiesAndReplaces(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("tar.gz path")
	}
	var tgz bytes.Buffer
	gz := gzip.NewWriter(&tgz)
	tw := tar.NewWriter(gz)
	body := []byte("#!/bin/sh\necho new\n")
	_ = tw.WriteHeader(&tar.Header{Name: "kubesage", Mode: 0o755, Size: int64(len(body)), Typeflag: tar.TypeReg})
	_, _ = tw.Write(body)
	tw.Close()
	gz.Close()
	name := ArchiveName("9.9.9", runtime.GOOS, runtime.GOARCH)
	sum := sha256.Sum256(tgz.Bytes())
	sums := hex.EncodeToString(sum[:]) + "  " + name + "\n"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch filepath.Base(r.URL.Path) {
		case "checksums.txt":
			w.Write([]byte(sums))
		case name:
			w.Write(tgz.Bytes())
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	DownloadBase = srv.URL + "/v{version}"
	exe := filepath.Join(t.TempDir(), "kubesage")
	os.WriteFile(exe, []byte("old"), 0o755)
	if err := Apply(context.Background(), "v9.9.9", exe); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(exe); !bytes.Equal(b, body) {
		t.Fatalf("binary not replaced: %q", b)
	}
	sums = strings.Repeat("0", 64) + "  " + name + "\n"
	if err := Apply(context.Background(), "9.9.9", exe); err == nil || !strings.Contains(err.Error(), "checksum") {
		t.Fatalf("tampered archive accepted: %v", err)
	}
	if ManagedBy("/opt/homebrew/Caskroom/kubesage/0.2.0/kubesage") == "" || ManagedBy("/usr/bin/kubesage") == "" || ManagedBy(exe) != "" {
		t.Fatal("package-manager detection wrong")
	}
}
