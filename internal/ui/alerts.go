package ui

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/gauravthakur02/kubesage/internal/config"
	"github.com/gauravthakur02/kubesage/internal/kube"
)

const alertsRes = "alerts"

// alert is one problem the background monitor found on the watch list.
type alert struct {
	key      string
	at       time.Time
	ctx      string
	issue    kube.Issue
	resolved time.Time
	seen     bool
}

type alertsMsg struct {
	found  map[string]alert // key → current problem
	failed []string
}

func pinLabel(p config.Pin) string {
	s := p.Context + " · " + p.Namespace
	if p.Name != "" {
		s += " · " + p.Kind + "/" + p.Name
	}
	return s
}

// pinCommand pins the selected workload, or the namespace with ":pin ns" (:unpin removes).
func (a *App) pinCommand(arg string, remove bool) tea.Cmd {
	ns := a.accessNamespace()
	pin := config.Pin{Context: a.client.Context, Namespace: ns}
	if arg != "ns" && arg != "namespace" {
		if row, ok := a.selected(); ok && a.res.Watchable() && a.res.Name != "namespaces" && a.res.Name != "events" {
			pin.Kind, pin.Name = a.res.Name, row.Name
			if row.Namespace != "" {
				pin.Namespace = row.Namespace
			}
		} else if row, ok := a.selected(); ok && a.res.Name == "namespaces" {
			pin.Namespace = row.Name
		}
	}
	list := a.cfg.Watchlist[:0:0]
	exists := false
	for _, p := range a.cfg.Watchlist {
		if p == pin {
			exists = true
			if remove {
				continue
			}
		}
		list = append(list, p)
	}
	switch {
	case remove && !exists:
		a.setStatus("not on the watch list: "+pinLabel(pin), true)
		return nil
	case !remove && exists:
		a.setStatus("already watched: "+pinLabel(pin), false)
		return nil
	case !remove:
		list = append(list, pin)
	}
	a.cfg.Watchlist = list
	if err := a.cfg.Save(); err != nil {
		a.setStatus("could not save the watch list: "+err.Error(), true)
		return nil
	}
	verb := "📌 watching "
	if remove {
		verb = "unpinned "
	}
	a.setStatus(verb+pinLabel(pin)+" · :alerts shows what it finds", false)
	return a.runMonitor()
}

func (a *App) alertInterval() time.Duration {
	if s := a.cfg.Alerts.IntervalSec; s >= 10 {
		return time.Duration(s) * time.Second
	}
	return 30 * time.Second
}

// runMonitor checks every pinned scope once in the background.
func (a *App) runMonitor() tea.Cmd {
	if len(a.cfg.Watchlist) == 0 || a.monitorBusy {
		return nil
	}
	a.monitorBusy, a.monitorAt = true, time.Now()
	pins := append([]config.Pin(nil), a.cfg.Watchlist...)
	certDays := a.cfg.Alerts.CertDays
	if certDays <= 0 {
		certDays = 14
	}
	connects := map[string]tea.Cmd{}
	for _, p := range pins {
		if _, ok := connects[p.Context]; !ok {
			connects[p.Context] = a.connect(p.Context, "", "probe")
		}
	}
	checkCerts := time.Since(a.certCheckAt) > 10*time.Minute
	if checkCerts {
		a.certCheckAt = time.Now()
	}
	prevCerts := a.certAlerts
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		out := alertsMsg{found: map[string]alert{}}
		clients := map[string]*kube.Client{}
		for name, cmd := range connects {
			m := cmd().(connectMsg)
			if m.err != nil {
				out.failed = append(out.failed, name+": "+m.err.Error())
				continue
			}
			clients[name] = m.client
		}
		scanned := map[string][]kube.Issue{}
		for _, p := range pins {
			c := clients[p.Context]
			if c == nil {
				continue
			}
			key := p.Context + "|" + p.Namespace
			issues, ok := scanned[key]
			if !ok {
				var err error
				issues, err = c.WithNamespace(p.Namespace).Scan(ctx)
				if err != nil {
					out.failed = append(out.failed, pinLabel(p)+": "+firstLineStr(err.Error(), 120))
				}
				scanned[key] = issues
			}
			for _, is := range issues {
				if p.Name != "" && is.Name != p.Name && !strings.HasPrefix(is.Name, p.Name+"-") {
					continue
				}
				k := p.Context + "/" + is.Kind + "/" + is.Namespace + "/" + is.Name + "/" + is.Reason
				out.found[k] = alert{key: k, at: time.Now(), ctx: p.Context, issue: is}
			}
			if p.Name == "" {
				if checkCerts {
					if certs, err := c.WithNamespace(p.Namespace).Certificates(ctx); err == nil {
						for _, ci := range certs {
							if !ci.NotAfter.IsZero() && ci.DaysLeft() < certDays {
								is := kube.Issue{Severity: kube.HealthWarn, Kind: "certificates", Namespace: ci.Namespace, Name: ci.Secret, Reason: "CertificateExpiring",
									Message: fmt.Sprintf("expires in %d days (%s)", ci.DaysLeft(), ci.NotAfter.Format("2006-01-02"))}
								if ci.DaysLeft() < 0 {
									is.Severity, is.Reason, is.Message = kube.HealthErr, "CertificateExpired", "expired on "+ci.NotAfter.Format("2006-01-02")
								}
								k := p.Context + "/cert/" + ci.Namespace + "/" + ci.Secret + "/" + is.Reason
								out.found[k] = alert{key: k, at: time.Now(), ctx: p.Context, issue: is}
							}
						}
					}
				} else {
					for k, al := range prevCerts {
						if strings.HasPrefix(k, p.Context+"/cert/"+p.Namespace+"/") {
							out.found[k] = al
						}
					}
				}
			}
		}
		return out
	}
}

// handleAlerts records new and resolved problems and notifies about new ones.
func (a *App) handleAlerts(m alertsMsg) {
	a.monitorBusy = false
	a.monitorErr = strings.Join(m.failed, "; ")
	if a.alerts == nil {
		a.alerts = map[string]*alert{}
	}
	a.certAlerts = map[string]alert{}
	var fresh []kube.Issue
	var freshCtx []string
	for k, al := range m.found {
		if strings.Contains(k, "/cert/") {
			a.certAlerts[k] = al
		}
		if cur, ok := a.alerts[k]; ok && cur.resolved.IsZero() {
			continue
		}
		al := al
		a.alerts[k] = &al
		a.alertOrder = append(a.alertOrder, k)
		fresh = append(fresh, al.issue)
		freshCtx = append(freshCtx, al.ctx)
	}
	for k, al := range a.alerts {
		if _, still := m.found[k]; !still && al.resolved.IsZero() {
			al.resolved = time.Now()
		}
	}
	if len(a.alertOrder) > 300 {
		for _, k := range a.alertOrder[:len(a.alertOrder)-300] {
			delete(a.alerts, k)
		}
		a.alertOrder = a.alertOrder[len(a.alertOrder)-300:]
	}
	if len(fresh) == 0 || !a.monitorPrimed {
		a.monitorPrimed = true // problems that already exist at startup do not ring
		return
	}
	a.routeAlerts(fresh, freshCtx)
	first := fresh[0]
	text := fmt.Sprintf("%s %s/%s: %s", freshCtx[0], first.Namespace, first.Name, first.Reason)
	if len(fresh) > 1 {
		text += fmt.Sprintf(" (+%d more)", len(fresh)-1)
	}
	a.setStatus("🔔 "+text+" · :alerts", true)
	if a.cfg.Alerts.Bell == nil || *a.cfg.Alerts.Bell {
		fmt.Fprint(os.Stderr, "\a")
	}
	if a.cfg.Alerts.Desktop == nil || *a.cfg.Alerts.Desktop {
		go notifyDesktop("kubesage", text)
	}
}

// notifyDesktop shows a desktop notification where a standard tool exists.
func notifyDesktop(title, text string) {
	switch runtime.GOOS {
	case "darwin":
		q := func(s string) string { return strings.ReplaceAll(strings.ReplaceAll(s, `\`, `\\`), `"`, `\"`) }
		_ = exec.Command("osascript", "-e", fmt.Sprintf(`display notification "%s" with title "%s"`, q(text), q(title))).Run()
	case "linux":
		if _, err := exec.LookPath("notify-send"); err == nil {
			_ = exec.Command("notify-send", title, text).Run()
		}
	}
}

// unseenAlerts counts active alerts not yet looked at.
func (a *App) unseenAlerts() int {
	n := 0
	for _, al := range a.alerts {
		if al.resolved.IsZero() && !al.seen {
			n++
		}
	}
	return n
}

func (a *App) alertsResource() kube.Resource {
	return kube.LocalResource(alertsRes, "Alert", "Alerts", func() kube.Table {
		t := kube.Table{Columns: []string{"SINCE", "STATE", "CLUSTER", "NAMESPACE", "OBJECT", "REASON", "MESSAGE"}}
		active := 0
		for i := len(a.alertOrder) - 1; i >= 0; i-- {
			al := a.alerts[a.alertOrder[i]]
			if al == nil {
				continue
			}
			state, h := "active", al.issue.Severity
			if !al.resolved.IsZero() {
				state, h = "resolved "+kube.Age(metav1Time(al.resolved))+" ago", kube.HealthNeutral
			} else {
				active++
			}
			t.Rows = append(t.Rows, kube.Row{Namespace: al.issue.Namespace, Name: al.key, Health: h, Cells: []string{
				kube.Age(metav1Time(al.at)) + " ago", state, al.ctx, al.issue.Namespace, strings.TrimSuffix(al.issue.Kind, "s") + "/" + al.issue.Name,
				al.issue.Reason, firstLineStr(al.issue.Message, 90)}})
		}
		pins := make([]string, 0, len(a.cfg.Watchlist))
		for _, p := range a.cfg.Watchlist {
			pins = append(pins, pinLabel(p))
		}
		t.Note = fmt.Sprintf("%d active · watching %d: %s", active, len(pins), kube.ShortList(pins, 3))
		if len(pins) == 0 {
			t.Note = "nothing pinned yet: select a workload and run :pin, or :pin ns for the whole namespace"
		}
		if a.monitorErr != "" {
			t.Note += " · ⚠ " + firstLineStr(a.monitorErr, 80)
		}
		return t
	})
}

func (a *App) openAlerts() tea.Cmd {
	for _, al := range a.alerts {
		al.seen = true
	}
	return a.openVirtual(a.alertsResource(), "enter: open the object · a: AI explain · :pin / :unpin to change the watch list")
}

func (a *App) alertRowAction(key string, row kube.Row) (tea.Cmd, bool) {
	al := a.alerts[row.Name]
	if al == nil {
		return nil, false
	}
	is := al.issue
	res, ok := kube.Lookup(is.Kind)
	switch key {
	case "enter":
		if al.ctx != a.client.Context {
			return a.runCommand(fmt.Sprintf("tabnew @%s %s", al.ctx, is.Namespace)), true
		}
		if !ok {
			return nil, true
		}
		return a.openText(res, is.Namespace, is.Name, "describe"), true
	case "r":
		return a.runbookFor(is.Reason + " " + is.Message + " " + is.Kind), true
	case "a":
		if al.ctx == a.client.Context && ok {
			return a.analyze(res, is.Namespace, is.Name), true
		}
		a.setStatus("switch to "+al.ctx+" first (enter opens it in a new tab)", true)
		return nil, true
	}
	return nil, false
}
