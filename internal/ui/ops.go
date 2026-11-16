package ui

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"sort"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/gauravthakur02/kubesage/internal/kube"
)

const (
	certsRes       = "tlscerts"
	imagesRes      = "images"
	storageRes     = "storage"
	gitopsRes      = "gitops"
	cronRunsPrefix = "cronruns:"
)

// openVirtual switches to a kubesage table and remembers where esc goes back to.
func (a *App) openVirtual(r kube.Resource, status string) tea.Cmd {
	back := a.res
	cmd := a.switchResource(r)
	if back.Name != r.Name {
		a.backTo = &back
	}
	a.setStatus(status, false)
	return cmd
}

// ---------- certificates ----------

func (a *App) certsResource() kube.Resource {
	c := a.client
	return kube.VirtualResource(certsRes, "Certificate", "Certs", func() kube.Table {
		ctx, cancel := context.WithTimeout(context.Background(), 2*timeout)
		defer cancel()
		t := kube.Table{Columns: []string{"NAMESPACE", "SECRET", "DAYS LEFT", "EXPIRES", "SUBJECT", "DNS NAMES", "ISSUER", "CERT-MANAGER"}}
		certs, err := c.Certificates(ctx)
		if err != nil {
			t.Note = "✗ " + err.Error()
			return t
		}
		soon, expired := 0, 0
		for _, ci := range certs {
			days, exp := "-", "-"
			if !ci.NotAfter.IsZero() {
				d := ci.DaysLeft()
				days, exp = fmt.Sprint(d), ci.NotAfter.Local().Format("2006-01-02")
				if d < 0 {
					days, expired = "EXPIRED", expired+1
				} else if d < 30 {
					soon++
				}
			}
			cm := ci.CertManager
			if cm != "" && ci.Ready != "" {
				cm += " · Ready=" + ci.Ready
			}
			if ci.Err != "" {
				cm = "✗ " + ci.Err
			}
			t.Rows = append(t.Rows, kube.Row{Namespace: ci.Namespace, Name: ci.Secret, Health: kube.CertHealth(ci), Cells: []string{
				ci.Namespace, ci.Secret, days, exp, ci.Subject, kube.ShortList(ci.DNSNames, 2), ci.Issuer, cm}})
		}
		t.Note = fmt.Sprintf("%d certificates · %d expire within 30 days · %d expired", len(certs), soon, expired)
		return t
	})
}

// ---------- images ----------

func (a *App) imagesResource() kube.Resource {
	c := a.client
	return kube.VirtualResource(imagesRes, "Image", "Images", func() kube.Table {
		ctx, cancel := context.WithTimeout(context.Background(), 2*timeout)
		defer cancel()
		t := kube.Table{Columns: []string{"IMAGE", "TAG", "REGISTRY", "PINNED", "PODS", "NAMESPACES", "WORKLOADS", "RUNNING DIGEST"}}
		imgs, err := c.Images(ctx)
		if err != nil {
			t.Note = "✗ " + err.Error()
			return t
		}
		latest, unpinned := 0, 0
		for _, ii := range imgs {
			h, pinned := kube.HealthOK, "digest"
			if !ii.Pinned {
				pinned = "tag"
				unpinned++
			}
			tag := ii.Tag
			if ii.Latest {
				h, tag = kube.HealthWarn, nonEmptyStr(tag, "(none)")+" ⚠"
				latest++
			}
			digest := ii.Digest
			if len(digest) > 19 {
				digest = digest[:19] + "…"
			}
			_, repo, _ := kube.SplitImage(ii.Image)
			t.Rows = append(t.Rows, kube.Row{Name: ii.Image, Health: h, Cells: []string{
				repo, tag, ii.Registry, pinned, fmt.Sprint(ii.Pods), kube.ShortList(ii.Namespaces, 2), kube.ShortList(ii.Workloads, 2), digest}})
		}
		t.Note = fmt.Sprintf("%d images · %d use latest or no tag · %d not pinned by digest", len(imgs), latest, unpinned)
		if _, err := exec.LookPath("trivy"); err == nil {
			t.Note += " · enter: Trivy scan"
		} else {
			t.Note += " · install trivy to scan for vulnerabilities"
		}
		return t
	})
}

type trivyReport struct {
	Results []struct {
		Target          string `json:"Target"`
		Vulnerabilities []struct {
			ID        string `json:"VulnerabilityID"`
			Pkg       string `json:"PkgName"`
			Installed string `json:"InstalledVersion"`
			Fixed     string `json:"FixedVersion"`
			Severity  string `json:"Severity"`
			Title     string `json:"Title"`
		} `json:"Vulnerabilities"`
	} `json:"Results"`
}

// scanImage runs Trivy (if installed) for HIGH and CRITICAL vulnerabilities.
func (a *App) scanImage(img string) tea.Cmd {
	if _, err := exec.LookPath("trivy"); err != nil {
		a.setStatus("Trivy is not installed (brew install trivy / scoop install trivy)", true)
		return nil
	}
	return a.openReport("trivy", "scanning "+img+" with Trivy (first run downloads the vulnerability DB)…", func(ctx context.Context) (string, string, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()
		out, err := exec.CommandContext(ctx, "trivy", "image", "--quiet", "--format", "json", "--scanners", "vuln", "--severity", "HIGH,CRITICAL", img).Output()
		if err != nil {
			msg := err.Error()
			if ee, ok := err.(*exec.ExitError); ok && len(ee.Stderr) > 0 {
				msg = strings.TrimSpace(string(ee.Stderr))
			}
			return "", "", fmt.Errorf("trivy: %s", msg)
		}
		var rep trivyReport
		if err := json.Unmarshal(out, &rep); err != nil {
			return "", "", err
		}
		s := a.st
		crit, high, fixable := 0, 0, 0
		var lines []string
		for _, r := range rep.Results {
			if len(r.Vulnerabilities) == 0 {
				continue
			}
			lines = append(lines, s.Title.Render("── "+r.Target))
			vs := r.Vulnerabilities
			sort.SliceStable(vs, func(i, j int) bool { return vs[i].Severity == "CRITICAL" && vs[j].Severity != "CRITICAL" })
			for _, v := range vs {
				st := s.WarnText
				if v.Severity == "CRITICAL" {
					st, crit = s.ErrText, crit+1
				} else {
					high++
				}
				fix := s.Muted.Render("no fix yet")
				if v.Fixed != "" {
					fix, fixable = s.OKText.Render("fixed in "+v.Fixed), fixable+1
				}
				lines = append(lines, fmt.Sprintf("  %s %-16s %s %s  %s  %s", st.Render(fmt.Sprintf("%-8s", v.Severity)), v.ID, v.Pkg, s.Muted.Render(v.Installed), fix, s.Muted.Render(v.Title)))
			}
		}
		if len(lines) == 0 {
			lines = []string{s.OKText.Render("✓ no HIGH or CRITICAL vulnerabilities found")}
		}
		return fmt.Sprintf("Trivy %s · %d critical · %d high · %d fixable", img, crit, high, fixable), strings.Join(lines, "\n"), nil
	})
}

// ---------- storage ----------

func (a *App) storageResource() kube.Resource {
	c := a.client
	return kube.VirtualResource(storageRes, "Volume", "Storage", func() kube.Table {
		ctx, cancel := context.WithTimeout(context.Background(), 2*timeout)
		defer cancel()
		t := kube.Table{Columns: []string{"NAMESPACE", "PVC", "STATUS", "CAPACITY", "USED", "USE%", "STORAGECLASS", "ACCESS", "MOUNTED BY"}}
		vols, err := c.Storage(ctx)
		if err != nil {
			t.Note = "✗ " + err.Error()
			return t
		}
		full, statsErr := 0, ""
		for _, v := range vols {
			h, used, pct := kube.HealthOK, "?", "?"
			if p := v.UsePercent(); p >= 0 {
				used, pct = kube.FormatBytes(v.Used), fmt.Sprintf("%d%%", p)
				switch {
				case p >= 90:
					h, full = kube.HealthErr, full+1
				case p >= 75:
					h = kube.HealthWarn
				}
			}
			switch v.Phase {
			case "Pending":
				h = kube.HealthWarn
			case "Lost":
				h = kube.HealthErr
			}
			mb := kube.ShortList(v.MountedBy, 2)
			if mb == "" {
				mb = "— not mounted"
				if h == kube.HealthOK {
					h = kube.HealthNeutral
				}
			}
			if v.StatsErr != "" {
				statsErr = v.StatsErr
			}
			t.Rows = append(t.Rows, kube.Row{Namespace: v.Namespace, Name: v.Name, Health: h, Cells: []string{
				v.Namespace, v.Name, v.Phase, bytesOrDash(v.Capacity), used, pct, v.StorageClass, v.Access, mb}})
		}
		t.Note = fmt.Sprintf("%d volumes · %d at 90%% or more", len(vols), full)
		if statsErr != "" {
			t.Note += " · usage unavailable (kubelet stats need nodes/proxy)"
		}
		return t
	})
}

// ---------- GitOps ----------

func (a *App) gitopsResource() kube.Resource {
	c := a.client
	return kube.VirtualResource(gitopsRes, "GitOps app", "GitOps", func() kube.Table {
		ctx, cancel := context.WithTimeout(context.Background(), 2*timeout)
		defer cancel()
		t := kube.Table{Columns: []string{"TOOL", "KIND", "NAMESPACE", "NAME", "SYNC", "HEALTH", "REVISION", "SOURCE", "MESSAGE"}}
		apps, found, err := c.GitOpsApps(ctx)
		if err != nil {
			t.Note = "✗ " + err.Error()
			return t
		}
		if !found {
			t.Note = "no Argo CD or Flux installed on this cluster"
			return t
		}
		bad := 0
		for _, ap := range apps {
			if ap.H == kube.HealthErr {
				bad++
			}
			t.Rows = append(t.Rows, kube.Row{Namespace: ap.Namespace, Name: ap.Kind + "/" + ap.Name, Health: ap.H, Cells: []string{
				ap.Tool, ap.Kind, ap.Namespace, ap.Name, ap.Sync, ap.Health, ap.Revision, ap.Source, firstLineStr(ap.Message, 80)}})
		}
		t.Note = fmt.Sprintf("%d apps · %d failing", len(apps), bad)
		return t
	})
}

func firstLineStr(s string, n int) string {
	s, _, _ = strings.Cut(s, "\n")
	if len(s) > n {
		s = s[:n] + "…"
	}
	return s
}

// ---------- CronJobs ----------

func (a *App) cronRunsResource(ns, name string) kube.Resource {
	c := a.client
	return kube.VirtualResource(cronRunsPrefix+ns+"/"+name, "Job run", "Runs", func() kube.Table {
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		t := kube.Table{Columns: []string{"JOB", "STATUS", "STARTED", "DURATION", "TRIGGER"}}
		runs, suspended, err := c.CronJobRuns(ctx, ns, name)
		if err != nil {
			t.Note = "✗ " + err.Error()
			return t
		}
		ok, failed := 0, 0
		var total time.Duration
		for _, r := range runs {
			trig := "schedule"
			if r.Manual {
				trig = "manual"
			}
			switch r.Health {
			case kube.HealthOK:
				ok++
				total += r.Duration()
			case kube.HealthErr:
				failed++
			}
			t.Rows = append(t.Rows, kube.Row{Namespace: ns, Name: r.Name, Health: r.Health, Cells: []string{
				r.Name, r.Status, kube.Age(metav1Time(r.Start)) + " ago", r.Duration().String(), trig}})
		}
		t.Note = fmt.Sprintf("%d runs kept · %d succeeded · %d failed", len(runs), ok, failed)
		if ok > 0 {
			t.Note += fmt.Sprintf(" · avg %s", (total / time.Duration(ok)).Round(time.Second))
		}
		if suspended {
			t.Note += " · SUSPENDED"
		}
		return t
	})
}

func isCronRuns(r kube.Resource) (ns, name string, ok bool) {
	rest, ok := strings.CutPrefix(r.Name, cronRunsPrefix)
	if !ok {
		return "", "", false
	}
	ns, name, _ = strings.Cut(rest, "/")
	return ns, name, true
}

func (a *App) cronRowAction(key string, row kube.Row) (tea.Cmd, bool) {
	ns, name, c := row.Namespace, row.Name, a.client
	switch key {
	case "h":
		return a.openVirtual(a.cronRunsResource(ns, name), "enter: job pods/logs · J: run now · esc: back"), true
	case "J":
		if !a.guardWrite() {
			return nil, true
		}
		a.askConfirm(fmt.Sprintf("Run cronjob %s/%s now on %s? (creates a Job from its template)", ns, name, c.Context), name, func() tea.Cmd {
			return func() tea.Msg {
				ctx, cancel := context.WithTimeout(context.Background(), timeout)
				defer cancel()
				job, err := c.TriggerCronJob(ctx, ns, name)
				return actionMsg{status: "started job " + job, err: err}
			}
		})
		return nil, true
	case "u":
		if !a.guardWrite() {
			return nil, true
		}
		suspended := len(row.Cells) > 2 && strings.Contains(strings.ToLower(strings.Join(row.Cells, " ")), "true")
		verb := "Suspend"
		if suspended {
			verb = "Resume"
		}
		a.askConfirm(fmt.Sprintf("%s cronjob %s/%s on %s?", verb, ns, name, c.Context), name, func() tea.Cmd {
			return runAction(strings.ToLower(verb)+"ed cronjob "+name, func(ctx context.Context) error {
				err := c.SuspendCronJob(ctx, ns, name, !suspended)
				return err
			})
		})
		return nil, true
	}
	return nil, false
}

func (a *App) cronRunsRowAction(key string, row kube.Row) (tea.Cmd, bool) {
	ns, cj, _ := isCronRuns(a.res)
	switch key {
	case "enter", "l":
		jobs, _ := kube.Lookup("jobs")
		if key == "l" {
			return fetchPods(a.client, jobs, row.Namespace, row.Name, "logs"), true
		}
		return a.openText(jobs, row.Namespace, row.Name, "describe"), true
	case "J":
		return a.cronRowAction("J", kube.Row{Namespace: ns, Name: cj})
	}
	return nil, false
}

// ---------- network policies ----------

func (a *App) openNetPol(ns, pod string) tea.Cmd {
	c := a.client
	pods, _ := kube.Lookup("pods")
	a.target = targetRef{pods, ns, pod}
	return a.openReport("netpol", "analysing network policies…", func(ctx context.Context) (string, string, error) {
		r, err := c.NetworkPolicies(ctx, ns, pod)
		if err != nil {
			return "", "", err
		}
		s := a.st
		var lines []string
		sec := func(t string) { lines = append(lines, "", s.Title.Render(t)) }
		if len(r.Policies) == 0 {
			lines = append(lines, s.WarnText.Render("No NetworkPolicy selects this pod: all ingress and egress traffic is allowed."))
		} else {
			lines = append(lines, s.Title.Render("Policies selecting this pod"))
			for _, p := range r.Policies {
				lines = append(lines, "  • "+p)
			}
		}
		sec("Ingress (who may connect to this pod)")
		switch {
		case !r.IngressIsolated:
			lines = append(lines, s.OKText.Render("  ✓ everything (not isolated for ingress)"))
		case len(r.Ingress) == 0:
			lines = append(lines, s.ErrText.Render("  ✗ nothing: ingress is denied by default"))
		default:
			for _, x := range r.Ingress {
				lines = append(lines, s.OKText.Render("  ✓ ")+x)
			}
		}
		sec("Egress (where this pod may connect)")
		switch {
		case !r.EgressIsolated:
			lines = append(lines, s.OKText.Render("  ✓ everything (not isolated for egress)"))
		case len(r.Egress) == 0:
			lines = append(lines, s.ErrText.Render("  ✗ nothing: egress is denied (DNS too, unless allowed)"))
		default:
			dns := false
			for _, x := range r.Egress {
				lines = append(lines, s.OKText.Render("  ✓ ")+x)
				if strings.Contains(x, "53/") || strings.Contains(x, "all ports") {
					dns = true
				}
			}
			if !dns {
				lines = append(lines, s.WarnText.Render("  ⚠ no rule allows port 53: DNS lookups will fail"))
			}
		}
		if r.AllNamespacePodCount > 0 {
			sec(fmt.Sprintf("Pods in %s that may connect: %d of %d", ns, len(r.PeersInNamespace), r.AllNamespacePodCount))
			for i, p := range r.PeersInNamespace {
				if i >= 15 {
					lines = append(lines, s.Muted.Render(fmt.Sprintf("  … and %d more", len(r.PeersInNamespace)-15)))
					break
				}
				lines = append(lines, "  "+p)
			}
			if r.BlockedInNamespace > 0 {
				lines = append(lines, s.ErrText.Render(fmt.Sprintf("  %d pods in this namespace are blocked", r.BlockedInNamespace)))
			}
			lines = append(lines, s.Muted.Render("  (namespace-selector peers are listed above as rules; they are not evaluated per pod here)"))
		}
		return fmt.Sprintf("network policies for pod %s/%s · %d policies", ns, pod, len(r.Policies)), strings.Join(strings.Split(strings.TrimPrefix(strings.Join(lines, "\n"), "\n"), "\n"), "\n"), nil
	})
}

// isKubesageTable reports whether the table is one of kubesage's own lists, where the
// generic resource actions (delete, scale, …) do not apply.
func (a *App) isKubesageTable() bool {
	if _, ok := a.currentScreen(); ok {
		return true
	}
	switch a.res.Name {
	case sizingRes, certsRes, imagesRes, storageRes, gitopsRes, alertsRes, policyRepRes, backupsRes, staleRes, scalingRes, kubeconfigRes, runbooksRes,
		secretsHealthRes, meshRes, costRes:
		return true
	}
	if strings.HasPrefix(a.res.Name, searchPrefix) {
		return true
	}
	_, _, ok := isCronRuns(a.res)
	return ok
}

// extraRowAction handles keys of the newer screens and CronJob/pod extras.
func (a *App) extraRowAction(key string, row kube.Row) (tea.Cmd, bool) {
	if sc, ok := a.currentScreen(); ok {
		if sc.action == nil {
			return nil, false
		}
		return sc.action(a, key, row)
	}
	switch {
	case a.res.Name == sizingRes:
		return a.sizingRowAction(key, row)
	case a.res.Name == alertsRes:
		return a.alertRowAction(key, row)
	case a.res.Name == staleRes:
		return a.staleRowAction(key, row)
	case a.res.Name == runbooksRes && key == "enter":
		if rbs, err := a.loadRunbooks(); err == nil {
			for _, rb := range rbs {
				if rb.Path == row.Name {
					return a.openRunbook(rb), true
				}
			}
		}
		return nil, true
	case a.res.Name == kubeconfigRes:
		return a.kubeconfigRowAction(key, row)
	case a.res.Name == scalingRes && key == "enter":
		if r, name, ok := policyTarget(row); ok {
			return a.openGraph(r.Kind, row.Namespace, name), true
		}
		return nil, true
	case a.res.Name == "pods" && key == "B":
		return a.browseFiles(row.Namespace, row.Name, "", "/"), true
	case a.res.Name == "pods" && key == "W":
		return a.openStartup(row.Namespace, row.Name), true
	case strings.HasPrefix(a.res.Name, searchPrefix):
		return a.searchRowAction(key, row)
	case a.res.Name == policyRepRes && key == "enter":
		kind, name, ok := strings.Cut(row.Name, "/")
		if r, found := kube.Lookup(strings.ToLower(kind)); ok && found {
			return a.openText(r, row.Namespace, name, "describe"), true
		}
		return nil, true
	case a.res.Name == backupsRes && key == "enter":
		return a.runManual("kubectl -n " + row.Namespace + " describe " + strings.ToLower(row.Cells[0]) + "s.velero.io " + row.Name), true
	case a.res.Name == helmRes && key == "u":
		return a.helmPreview(row.Namespace, row.Name, ""), true
	case a.res.Name == "cronjobs":
		return a.cronRowAction(key, row)
	case a.res.Name == "serviceaccounts" && key == "p":
		return a.openSAPermissions(row.Namespace, row.Name), true
	case a.res.Name == "pods" && key == "N":
		return a.openNetCheck(row.Namespace, row.Name, ""), true
	case a.res.Name == "pods" && key == "P":
		return a.openNetPol(row.Namespace, row.Name), true
	case a.res.Name == certsRes && key == "enter":
		secrets, _ := kube.Lookup("secrets")
		return a.openText(secrets, row.Namespace, row.Name, "yaml"), true
	case a.res.Name == imagesRes && key == "enter":
		return a.scanImage(row.Name), true
	case a.res.Name == storageRes && (key == "enter" || key == "d"):
		pvc, _ := kube.Lookup("pvc")
		return a.openText(pvc, row.Namespace, row.Name, "describe"), true
	case a.res.Name == gitopsRes && key == "enter":
		kind, name, _ := strings.Cut(row.Name, "/")
		cells := row.Cells
		lines := []string{a.st.Title.Render(cells[0] + " " + kind + " " + row.Namespace + "/" + name), ""}
		for i, c := range a.table.Columns {
			if i < len(cells) {
				lines = append(lines, fmt.Sprintf("  %-10s %s", c, cells[i]))
			}
		}
		lines = append(lines, "", a.st.Muted.Render("Objects of this app carry a GitOps marker: safe edit warns that changes there are reverted on the next sync."))
		a.prevMode, a.mode = a.mode, modeText
		a.textKind, a.textTitle, a.textLines = "gitops", kind+" "+name, lines
		a.renderText(true)
		return nil, true
	}
	if _, _, ok := isCronRuns(a.res); ok {
		return a.cronRunsRowAction(key, row)
	}
	return nil, false
}

// kubesageTitle is the table title of the newer screens ("" when not one of them).
func (a *App) kubesageTitle() string {
	note := a.table.Note
	if sc, ok := a.currentScreen(); ok && sc.title != nil {
		return sc.title(a)
	}
	switch a.res.Name {
	case sizingRes:
		return "Right-sizing · " + a.client.NamespaceLabel() + " · " + note
	case certsRes:
		return "TLS certificates · " + a.client.NamespaceLabel() + " · " + note
	case imagesRes:
		return "Images in use · " + a.client.NamespaceLabel() + " · " + note
	case storageRes:
		return "Storage · " + a.client.NamespaceLabel() + " · " + note
	case gitopsRes:
		return "GitOps apps · " + a.client.NamespaceLabel() + " · " + note
	case alertsRes:
		return "Alerts · " + note
	case staleRes:
		return "Restart needed · " + a.client.NamespaceLabel() + " · " + note
	case scalingRes:
		return "Autoscaling · " + a.client.NamespaceLabel() + " · " + note
	case kubeconfigRes:
		return "Kubeconfig · " + note
	case runbooksRes:
		return "Runbooks · " + note
	case secretsHealthRes:
		return "Secrets health · " + a.client.NamespaceLabel() + " · " + note
	case meshRes:
		return "Service mesh · " + note
	case costRes:
		return "Cost · " + note
	case policyRepRes:
		return "Policy reports · " + a.client.NamespaceLabel() + " · " + note
	case backupsRes:
		return "Velero backups · " + note
	}
	if ns, name, ok := isCronRuns(a.res); ok {
		return fmt.Sprintf("Runs of cronjob %s/%s · %s", ns, name, note)
	}
	if q, ok := strings.CutPrefix(a.res.Name, searchPrefix); ok {
		return "Search " + q + " · " + note
	}
	return ""
}

// kubesageHints are the footer hints of the newer screens.
func (a *App) kubesageHints() []kv {
	if sc, ok := a.currentScreen(); ok {
		return sc.hints
	}
	switch a.res.Name {
	case sizingRes:
		return []kv{{"enter", "suggested change"}, {"e", "safe edit"}, {"y", "yaml"}, {":export csv", "export"}, {"esc", "back"}}
	case certsRes:
		return []kv{{"enter", "secret (masked)"}, {"/", "filter"}, {"esc", "back"}}
	case imagesRes:
		return []kv{{"enter", "Trivy scan"}, {"/", "filter"}, {"esc", "back"}}
	case storageRes:
		return []kv{{"enter", "describe"}, {"/", "filter"}, {"esc", "back"}}
	case gitopsRes:
		return []kv{{"enter", "details"}, {"/", "filter"}, {"esc", "back"}}
	case policyRepRes, backupsRes:
		return []kv{{"enter", "describe"}, {"/", "filter"}, {"esc", "back"}}
	case scalingRes:
		return []kv{{"enter", "target relationships"}, {"/", "filter"}, {"esc", "back"}}
	case kubeconfigRes:
		return []kv{{"enter", "open"}, {"p", "check"}, {"r", "rename"}, {"D", "delete"}, {"a", "add AKS cluster"}, {"esc", "back"}}
	case staleRes:
		return []kv{{"R", "rollout restart"}, {"enter", "relationships"}, {"/", "filter"}, {"esc", "back"}}
	case alertsRes:
		return []kv{{"enter", "open object"}, {"a", "AI explain"}, {"r", "runbook"}, {":pin/:unpin", "watch list"}, {"esc", "back"}}
	}
	if strings.HasPrefix(a.res.Name, searchPrefix) {
		return []kv{{"enter", "open"}, {"y", "yaml"}, {"/", "filter"}, {"esc", "back"}}
	}
	if _, _, ok := isCronRuns(a.res); ok {
		return []kv{{"enter", "describe job"}, {"l", "logs"}, {"J", "run now"}, {"esc", "back"}}
	}
	return nil
}
