package ui

import (
	"regexp"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/gauravthakur02/kubesage/internal/config"
	"github.com/gauravthakur02/kubesage/internal/kube"
)

func TestSlowTablesRefreshLessOften(t *testing.T) {
	a, _ := newShopApp(t, "pods", true)
	run(a, a.runCommand("certs"), 0)
	if !a.res.Slow {
		t.Fatal("certs should be a slow table")
	}
	gen := a.listGen
	a.Update(tickMsg(time.Now()))
	if a.listGen != gen {
		t.Fatal("slow table re-queried on a normal tick")
	}
	a.lastList = time.Now().Add(-time.Minute)
	a.Update(tickMsg(time.Now()))
	if a.listGen == gen {
		t.Fatal("slow table not refreshed after slow_refresh_seconds")
	}
	mustContain(t, screen(a), "↻ 30s")
}

func TestEventHistoryScreenAndRecording(t *testing.T) {
	a, _ := newShopApp(t, "pods", true)
	_, err := a.eventStore().Record("dev", []kube.StoredEvent{{UID: "u1", Namespace: "shop", Kind: "Pod", Name: "web-old",
		Type: "Warning", Reason: "BackOff", Message: "restarting failed container", Last: time.Now().Add(-26 * time.Hour), Count: 4}})
	if err != nil {
		t.Fatal(err)
	}
	run(a, a.runCommand("history"), 0)
	mustContain(t, screen(a), "Event history", "web-old", "BackOff", "26h ago")
	cmd := a.recordEvents()
	if cmd == nil {
		t.Fatal("no recording on first tick")
	}
	if m := cmd().(eventsRecordedMsg); m.err != nil {
		t.Fatal(m.err)
	}
	a.evBusy = false
	if a.recordEvents() != nil {
		t.Fatal("recorded again within a minute")
	}
	a.cfg.EventHistory.Off = true
	a.evAt = time.Time{}
	if a.recordEvents() != nil {
		t.Fatal("recorded while off")
	}
}

func TestFleetView(t *testing.T) {
	a, _ := newShopApp(t, "pods", true)
	run(a, a.runCommand("fleet"), 0)
	v := screen(a)
	mustContain(t, v, "Fleet", "dev", "WARN EVENTS")
	if a.res.Name != fleetRes || len(a.table.Rows) != 1 {
		t.Fatalf("fleet rows = %+v", a.table.Rows)
	}
}

func TestSchedulesScreen(t *testing.T) {
	a, cs := newShopApp(t, "pods", false)
	a.cfg.Schedules = []config.Schedule{{Name: "nights", Context: "^dev$", Namespace: "shop", Down: "00:00", Up: "00:01", Days: []string{"mon", "tue", "wed", "thu", "fri", "sat", "sun"}}}
	run(a, a.runCommand("schedules"), 0)
	mustContain(t, screen(a), "Schedules", "nights", "running")
	press(a, "d")
	if a.ov != ovConfirm {
		t.Fatal("scale down did not ask")
	}
	press(a, "y")
	for i := 0; i < 3; i++ {
		run(a, a.refresh(), 0)
	}
	d, _ := cs.AppsV1().Deployments("shop").List(t.Context(), metav1ListAll())
	for _, x := range d.Items {
		if *x.Spec.Replicas != 0 {
			t.Fatalf("%s still has %d replicas", x.Name, *x.Spec.Replicas)
		}
	}
	a.cfg.Schedules[0].Context = ".*"
	a.cfg.DangerContexts = "dev"
	if why := scheduleBlocked(a.cfg, a.cfg.Schedules[0], "dev"); why == "" {
		t.Fatal("danger context not blocked")
	}
}

func TestChaosGuarded(t *testing.T) {
	a, _ := newShopApp(t, "deployments", true)
	run(a, a.openChaos(), 0)
	if a.ov == ovConfirm || !strings.Contains(a.status, "read-only") {
		t.Fatalf("chaos allowed in read-only: %q", a.status)
	}
	a2, _ := newShopApp(t, "deployments", false)
	a2.cfg.DangerContexts = "dev"
	a2.dangerRe = mustRe(a2.cfg.DangerContexts)
	run(a2, a2.openChaos(), 0)
	if !strings.Contains(a2.status, "production-like") {
		t.Fatalf("chaos allowed on danger context: %q", a2.status)
	}
}

func TestShareCommand(t *testing.T) {
	a, _ := newShopApp(t, "pods", true)
	a.shareCommand("")
	if a.sharing == nil || !strings.Contains(a.status, "127.0.0.1") {
		t.Fatalf("share did not start: %q", a.status)
	}
	_ = a.View()
	a.shareCommand("stop")
	if a.sharing != nil {
		t.Fatal("share did not stop")
	}
	_, _ = a.Update(tea.KeyMsg{Type: tea.KeyEsc})
}

func metav1ListAll() metav1.ListOptions { return metav1.ListOptions{} }

func mustRe(s string) *regexp.Regexp { return regexp.MustCompile(s) }
