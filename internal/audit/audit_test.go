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
	r, err := Verify(p)
	if err != nil || r.Checked != 3 || r.Legacy != 1 || r.Signed != 0 {
		t.Fatalf("verify = %+v %v", r, err)
	}
	b, _ := os.ReadFile(p)
	os.WriteFile(p, []byte(strings.Replace(string(b), `"action":"delete"`, `"action":"scale"`, 1)), 0o600)
	if _, err := Verify(p); err == nil || !strings.Contains(err.Error(), "modified") {
		t.Fatalf("tampering not detected: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	os.WriteFile(p, []byte(strings.Join(append(lines[:2], lines[3:]...), "\n")+"\n"), 0o600)
	if _, err := Verify(p); err == nil || !strings.Contains(err.Error(), "chain broken") {
		t.Fatalf("deletion not detected: %v", err)
	}
	// removing the oldest chained entries (the head of the chain) is detected too
	os.WriteFile(p, []byte(strings.Join(lines[2:], "\n")+"\n"), 0o600)
	if _, err := Verify(p); err == nil || !strings.Contains(err.Error(), "chain broken") {
		t.Fatalf("prefix deletion not detected: %v", err)
	}
}

func TestKeyedChain(t *testing.T) {
	p := filepath.Join(t.TempDir(), "audit.log")
	t.Setenv(KeyEnv, "")
	Append(p, Entry{Time: time.Now(), Context: "dev", Action: "scale", By: "user", Result: "ok"})
	t.Setenv(KeyEnv, "k1")
	for _, a := range []string{"delete", "edit.apply"} {
		if err := Append(p, Entry{Time: time.Now(), Context: "dev", Action: a, By: "user", Result: "ok"}); err != nil {
			t.Fatal(err)
		}
	}
	if r, err := Verify(p); err != nil || r.Checked != 3 || r.Signed != 2 {
		t.Fatalf("verify = %+v %v", r, err)
	}
	t.Setenv(KeyEnv, "")
	if _, err := Verify(p); err == nil || !strings.Contains(err.Error(), KeyEnv) {
		t.Fatalf("signed log verified without the key: %v", err)
	}
	t.Setenv(KeyEnv, "wrong")
	if _, err := Verify(p); err == nil {
		t.Fatal("wrong key accepted")
	}
	// someone without the key edits a signed entry and re-hashes it: detected
	t.Setenv(KeyEnv, "k1")
	b, _ := os.ReadFile(p)
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	var e Entry
	json.Unmarshal([]byte(lines[2]), &e)
	e.Action = "scale"
	e.Keyed = false
	e.Hash = e.digest(nil)
	j, _ := json.Marshal(e)
	lines[2] = string(j)
	os.WriteFile(p, []byte(strings.Join(lines, "\n")+"\n"), 0o600)
	if _, err := Verify(p); err == nil || !strings.Contains(err.Error(), "rewritten") {
		t.Fatalf("unsigned forgery after signed entries not detected: %v", err)
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
