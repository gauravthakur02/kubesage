package audit

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestChainVerifyAndTamper(t *testing.T) {
	p := filepath.Join(t.TempDir(), "audit.log")
	os.WriteFile(p, []byte(`{"time":"2026-01-01T00:00:00Z","context":"dev","action":"old","by":"user","result":"ok"}`+"\n"), 0o600)
	for i, a := range []string{"scale", "delete", "edit.apply"} {
		if err := Append(p, Entry{Time: time.Now().Add(time.Duration(i) * time.Second), Context: "dev", Action: a, By: "user", Result: "ok"}); err != nil {
			t.Fatal(err)
		}
	}
	n, legacy, err := Verify(p)
	if err != nil || n != 3 || legacy != 1 {
		t.Fatalf("verify = %d %d %v", n, legacy, err)
	}
	b, _ := os.ReadFile(p)
	os.WriteFile(p, []byte(strings.Replace(string(b), `"action":"delete"`, `"action":"scale"`, 1)), 0o600)
	if _, _, err := Verify(p); err == nil || !strings.Contains(err.Error(), "modified") {
		t.Fatalf("tampering not detected: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	os.WriteFile(p, []byte(strings.Join(append(lines[:2], lines[3:]...), "\n")+"\n"), 0o600)
	if _, _, err := Verify(p); err == nil || !strings.Contains(err.Error(), "chain broken") {
		t.Fatalf("deletion not detected: %v", err)
	}
}

func TestExportLoki(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&got)
		w.WriteHeader(204)
	}))
	defer srv.Close()
	es := []Entry{{Time: time.Now(), Context: "dev", Action: "scale"}, {Time: time.Now().Add(-48 * time.Hour), Context: "dev", Action: "old"}}
	n, err := ExportLoki(context.Background(), srv.URL, es, time.Now().Add(-time.Hour), nil)
	if err != nil || n != 1 || len(got["streams"].([]any)) != 1 {
		t.Fatalf("export = %d %v %v", n, err, got)
	}
}
