package kube

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// StoredEvent is a Kubernetes event kept on local disk, so history survives the API server's ~1h event TTL.
type StoredEvent struct {
	UID       string    `json:"uid"`
	Namespace string    `json:"ns"`
	Kind      string    `json:"kind"`
	Name      string    `json:"name"`
	Type      string    `json:"type"`
	Reason    string    `json:"reason"`
	Message   string    `json:"msg"`
	First     time.Time `json:"first"`
	Last      time.Time `json:"last"`
	Count     int32     `json:"count"`
}

// EventStore appends events per kube-context to <Dir>/<context>.jsonl.
type EventStore struct {
	Dir  string
	mu   sync.Mutex
	seen map[string]map[string]int32 // context -> uid -> last stored count
}

var unsafeFile = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

func (s *EventStore) file(kctx string) string {
	return filepath.Join(s.Dir, unsafeFile.ReplaceAllString(kctx, "_")+".jsonl")
}

// FromEvent converts an API event.
func FromEvent(e corev1.Event) StoredEvent {
	last := e.LastTimestamp.Time
	if last.IsZero() {
		last = e.EventTime.Time
	}
	if last.IsZero() {
		last = e.CreationTimestamp.Time
	}
	first := e.FirstTimestamp.Time
	if first.IsZero() {
		first = last
	}
	n := e.Count
	if n == 0 {
		n = 1
	}
	if e.Series != nil && e.Series.Count > n {
		n = e.Series.Count
	}
	return StoredEvent{UID: string(e.UID), Namespace: e.Namespace, Kind: e.InvolvedObject.Kind, Name: e.InvolvedObject.Name,
		Type: e.Type, Reason: e.Reason, Message: firstLine(e.Message), First: first, Last: last, Count: n}
}

// Record appends events that are new or whose count grew since they were last stored. It returns how many were written.
func (s *EventStore) Record(kctx string, evs []StoredEvent) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.seen == nil {
		s.seen = map[string]map[string]int32{}
	}
	seen := s.seen[kctx]
	if seen == nil {
		seen = map[string]int32{}
		if old, err := s.loadLocked(kctx, time.Time{}); err == nil {
			for _, e := range old {
				seen[e.UID] = e.Count
			}
		}
		s.seen[kctx] = seen
	}
	var fresh []StoredEvent
	for _, e := range evs {
		if e.UID == "" {
			continue
		}
		if c, ok := seen[e.UID]; ok && c >= e.Count {
			continue
		}
		fresh = append(fresh, e)
	}
	if len(fresh) == 0 {
		return 0, nil
	}
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		return 0, err
	}
	f, err := os.OpenFile(s.file(kctx), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	w := bufio.NewWriter(f)
	for _, e := range fresh {
		b, _ := json.Marshal(e)
		w.Write(append(b, '\n'))
		seen[e.UID] = e.Count
	}
	return len(fresh), w.Flush()
}

// Load returns stored events last seen after since, one per UID (latest count), newest first.
func (s *EventStore) Load(kctx string, since time.Time) ([]StoredEvent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.loadLocked(kctx, since)
}

func (s *EventStore) loadLocked(kctx string, since time.Time) ([]StoredEvent, error) {
	f, err := os.Open(s.file(kctx))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	byUID := map[string]StoredEvent{}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		var e StoredEvent
		if json.Unmarshal(sc.Bytes(), &e) != nil || e.Last.Before(since) {
			continue
		}
		if old, ok := byUID[e.UID]; !ok || e.Count >= old.Count {
			byUID[e.UID] = e
		}
	}
	out := make([]StoredEvent, 0, len(byUID))
	for _, e := range byUID {
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Last.After(out[j].Last) })
	return out, sc.Err()
}

// Prune rewrites the file keeping only events last seen within keep.
func (s *EventStore) Prune(kctx string, keep time.Duration) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	evs, err := s.loadLocked(kctx, time.Now().Add(-keep))
	if err != nil || evs == nil {
		return err
	}
	tmp := s.file(kctx) + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	w := bufio.NewWriter(f)
	for i := len(evs) - 1; i >= 0; i-- {
		b, _ := json.Marshal(evs[i])
		w.Write(append(b, '\n'))
	}
	if err := w.Flush(); err != nil {
		f.Close()
		return err
	}
	f.Close()
	return os.Rename(tmp, s.file(kctx))
}

// CollectEvents lists the current events in the client's namespace (or every allowed namespace).
func (c *Client) CollectEvents(ctx context.Context) ([]StoredEvent, error) {
	return listAcross(ctx, c, c.Namespace, func(ns string) ([]StoredEvent, error) {
		list, err := c.Clientset.CoreV1().Events(ns).List(ctx, metav1.ListOptions{})
		if err != nil {
			return nil, err
		}
		out := make([]StoredEvent, 0, len(list.Items))
		for _, e := range list.Items {
			out = append(out, FromEvent(e))
		}
		return out, nil
	})
}

// EventHealth grades a stored event like the timeline does.
func EventHealth(typ, reason string) Health {
	if typ != corev1.EventTypeWarning {
		return HealthNeutral
	}
	if strings.Contains(reason, "Fail") || strings.Contains(reason, "BackOff") || reason == "OOMKilling" || reason == "Unhealthy" {
		return HealthErr
	}
	return HealthWarn
}
