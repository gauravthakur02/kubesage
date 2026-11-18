// Package audit is kubeweave's change log: JSON lines, each entry chained to the previous one by a SHA-256
// hash so that edited or deleted lines are detected (`kubeweave audit verify`).
package audit

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"
)

// Entry is one change made through kubeweave (never secret values: only action names and targets).
type Entry struct {
	Time    time.Time `json:"time"`
	Context string    `json:"context"`
	NS      string    `json:"namespace,omitempty"`
	Action  string    `json:"action"`
	Target  string    `json:"target,omitempty"`
	By      string    `json:"by"` // user or ai
	Reason  string    `json:"reason,omitempty"`
	Result  string    `json:"result"`
	Prev    string    `json:"prev,omitempty"` // hash of the previous entry
	Hash    string    `json:"hash,omitempty"` // sha256(prev + this entry without hash)
}

var mu sync.Mutex

func (e Entry) digest() string {
	e.Hash = ""
	b, _ := json.Marshal(e)
	sum := sha256.Sum256(append([]byte(e.Prev), b...))
	return hex.EncodeToString(sum[:])
}

// lastHash reads the hash of the last entry (only the tail of the file is read).
func lastHash(f *os.File) string {
	st, err := f.Stat()
	if err != nil || st.Size() == 0 {
		return ""
	}
	n := min(st.Size(), 64*1024)
	buf := make([]byte, n)
	if _, err := f.ReadAt(buf, st.Size()-n); err != nil && err != io.EOF {
		return ""
	}
	lines := bytes.Split(bytes.TrimRight(buf, "\n"), []byte("\n"))
	var e Entry
	if json.Unmarshal(lines[len(lines)-1], &e) != nil {
		return ""
	}
	return e.Hash
}

// Append chains and writes one entry.
func Append(path string, e Entry) error {
	mu.Lock()
	defer mu.Unlock()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	e.Prev = lastHash(f)
	e.Hash = e.digest()
	b, _ := json.Marshal(e)
	_, err = f.Write(append(b, '\n'))
	return err
}

// Read returns every entry.
func Read(path string) ([]Entry, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []Entry
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		var e Entry
		if json.Unmarshal(sc.Bytes(), &e) == nil {
			out = append(out, e)
		}
	}
	return out, sc.Err()
}

// Verify checks the hash chain. Entries written before chaining existed (no hash) are counted as legacy.
func Verify(path string) (checked, legacy int, err error) {
	entries, err := Read(path)
	if err != nil {
		return 0, 0, err
	}
	prev := ""
	for i, e := range entries {
		if e.Hash == "" {
			legacy++
			continue
		}
		if e.Prev != prev { // the first chained entry has no predecessor, so a removed head is caught too
			return checked, legacy, fmt.Errorf("entry %d (%s %s): chain broken, an entry before it was changed or removed", i+1, e.Time.Format(time.RFC3339), e.Action)
		}
		if e.digest() != e.Hash {
			return checked, legacy, fmt.Errorf("entry %d (%s %s): content was modified", i+1, e.Time.Format(time.RFC3339), e.Action)
		}
		prev = e.Hash
		checked++
	}
	return checked, legacy, nil
}

// ExportLoki pushes entries newer than since to a Grafana Loki push endpoint (…/loki/api/v1/push).
func ExportLoki(ctx context.Context, url string, entries []Entry, since time.Time, headers map[string]string) (int, error) {
	type stream struct {
		Stream map[string]string `json:"stream"`
		Values [][2]string       `json:"values"`
	}
	byCtx := map[string]*stream{}
	n := 0
	for _, e := range entries {
		if !e.Time.After(since) {
			continue
		}
		s := byCtx[e.Context]
		if s == nil {
			s = &stream{Stream: map[string]string{"app": "kubeweave", "kind": "audit", "context": e.Context}}
			byCtx[e.Context] = s
		}
		line, _ := json.Marshal(e)
		s.Values = append(s.Values, [2]string{strconv.FormatInt(e.Time.UnixNano(), 10), string(line)})
		n++
	}
	if n == 0 {
		return 0, nil
	}
	body := struct {
		Streams []*stream `json:"streams"`
	}{}
	for _, s := range byCtx {
		body.Streams = append(body.Streams, s)
	}
	b, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(b))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return 0, fmt.Errorf("loki answered %s", resp.Status)
	}
	return n, nil
}
