package kube

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

// Read-only smoke test against a real cluster:
//
//	KUBEWEAVE_LIVE_CONTEXT=my-ctx go test ./internal/kube -run Live -v
func TestLiveReadOnly(t *testing.T) {
	kctx := os.Getenv("KUBEWEAVE_LIVE_CONTEXT")
	if kctx == "" {
		t.Skip("set KUBEWEAVE_LIVE_CONTEXT")
	}
	c, err := New("", kctx, "")
	if err != nil {
		t.Fatal(err)
	}
	c.Namespace = AllNamespaces
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	v, err := c.ServerVersion()
	t.Logf("server %s err=%v", v, err)
	for _, r := range Resources() {
		start := time.Now()
		tbl, err := r.List(ctx, c)
		if err != nil {
			t.Errorf("%s: %v", r.Name, err)
			continue
		}
		t.Logf("%-26s %4d rows  %v", r.Name, len(tbl.Rows), time.Since(start).Round(time.Millisecond))
	}
	p, err := c.Pulse(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("pulse: %+v", p)

	// AI context for the first deployment, without calling any model.
	dep, _ := Lookup("deployments")
	tbl, _ := dep.List(ctx, c)
	if len(tbl.Rows) > 0 {
		row := tbl.Rows[0]
		data, err := c.GatherContext(ctx, dep, row.Namespace, row.Name, true, 20)
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("context for %s/%s: %d bytes", row.Namespace, row.Name, len(data))
	}
}

// KUBEWEAVE_LIVE_CONTEXT=ctx KUBEWEAVE_LIVE_PF=ns/svc go test ./internal/kube -run LivePortForward -v
func TestLivePortForward(t *testing.T) {
	kctx, target := os.Getenv("KUBEWEAVE_LIVE_CONTEXT"), os.Getenv("KUBEWEAVE_LIVE_PF")
	if kctx == "" || target == "" {
		t.Skip("set KUBEWEAVE_LIVE_CONTEXT and KUBEWEAVE_LIVE_PF=namespace/service")
	}
	ns, name, _ := strings.Cut(target, "/")
	c, err := New("", kctx, ns)
	if err != nil {
		t.Fatal(err)
	}
	svc, _ := Lookup("svc")
	targets, err := c.PortTargets(context.Background(), svc, ns, name)
	if err != nil {
		t.Fatal(err)
	}
	for _, tg := range targets {
		t.Logf("target: %s via pod %s", tg.Label(), tg.Pod)
	}
	f, err := c.PortForward(targets[0], 0, targets[0].Port)
	if err != nil {
		t.Logf("port-forward error (expected without RBAC): %v", err)
		return
	}
	defer RemoveForward(f.ID)
	t.Logf("forwarding %s", f)
	resp, err := http.Get(fmt.Sprintf("http://localhost:%d/", f.Local))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	t.Logf("HTTP %d through the tunnel", resp.StatusCode)
}
