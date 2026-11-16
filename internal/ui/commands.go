package ui

import (
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/gauravthakur02/kubesage/internal/kube"
)

// runCommand handles the : command line.

func (a *App) runCommand(line string) tea.Cmd {
	if rest, ok := strings.CutPrefix(strings.TrimSpace(line), "!"); ok {
		return a.runManual(rest)
	}
	fields := strings.Fields(line)
	if len(fields) == 0 {
		return nil
	}
	cmd, arg := strings.ToLower(fields[0]), ""
	if len(fields) > 1 {
		arg = fields[1]
	}
	if c, ok := a.tabCommand(cmd, strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), fields[0]))); ok {
		return c
	}
	switch cmd {
	case "q", "q!", "quit", "exit":
		a.quitCleanup()
		return tea.Quit
	case "ai-key", "aikey", "key":
		name := arg
		if name == "" {
			name = a.profile
		}
		return a.promptKey(name)
	case "ai-add", "aiadd":
		return a.addProfileWizard()
	case "ai-rm", "ai-remove":
		if arg == "" {
			a.setStatus("usage: :ai-rm <profile>", true)
			return nil
		}
		a.askConfirm("Remove AI profile "+arg+" and its stored key?", "", func() tea.Cmd {
			if err := a.cfg.RemoveProfile(arg); err != nil {
				a.setStatus(err.Error(), true)
				return nil
			}
			_ = a.cfg.Save()
			if a.profile == arg {
				a.setProfile(a.cfg.AI.Active)
			}
			a.setStatus("removed AI profile "+arg, false)
			return nil
		})
		return nil
	case "taint":
		return a.taintCommand(strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), fields[0])))
	case "view", "views":
		return a.viewCommand(strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), fields[0])))
	case "debug":
		if row, ok := a.selected(); ok && a.res.Name == "pods" {
			if !a.guardWrite() {
				return nil
			}
			return fetchPods(a.client, a.res, row.Namespace, row.Name, "debug")
		}
		a.setStatus("select a pod first (:debug starts an ephemeral debug container)", true)
		return nil
	case "live":
		switch arg {
		case "watch", "poll":
			a.cfg.LiveUpdates = arg
		default:
			if a.liveWatch() {
				a.cfg.LiveUpdates = "poll"
			} else {
				a.cfg.LiveUpdates = "watch"
			}
		}
		_ = a.cfg.Save()
		a.setStatus("live updates: "+a.cfg.LiveUpdates, false)
		return tea.Batch(a.ensureWatches()...)
	case "helm", "releases":
		if arg == "charts" || arg == "chart" {
			return a.openCharts()
		}
		return a.openHelm()
	case "charts", "chart":
		return a.openCharts()
	case "incident", "sos":
		return a.openIncident()
	case "rightsize", "rightsizing", "sizing", "cost":
		return a.openSizing()
	case "certs", "tls", "certificates-expiry":
		return a.openVirtual(a.certsResource(), "TLS certificates, soonest expiry first · enter: secret (masked) · red = expired or failing, yellow = under 30 days")
	case "images", "image":
		return a.openVirtual(a.imagesResource(), "images in use · enter: Trivy vulnerability scan · w: workloads using it")
	case "storage", "volumes", "pvcusage":
		return a.openVirtual(a.storageResource(), "PVCs by usage · enter: describe · w: workload using it")
	case "gitops", "argo", "argocd", "flux":
		return a.openVirtual(a.gitopsResource(), "Argo CD and Flux apps · enter: details")
	case "netpol", "netpols", "reach":
		if a.res.Name == "pods" {
			if row, ok := a.selected(); ok {
				return a.openNetPol(row.Namespace, row.Name)
			}
		}
		a.setStatus("select a pod first, then :netpol (or press P on it)", true)
		return nil
	case "diff", "compare":
		return a.compareCommand(strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), fields[0])))
	case "export":
		return a.exportTable(strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), fields[0])))
	case "audit", "changes":
		return a.openAudit()
	case "upgrade", "upgrades", "deprecations":
		return a.openUpgrade(arg)
	case "rbac", "access", "cani", "can-i":
		return a.openAccess()
	case "whocan", "who-can":
		return a.openWhoCan(strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), fields[0])))
	case "why":
		return a.openWhy()
	case "secrets-health", "secretshealth", "rotation", "externalsecrets":
		return a.openVirtual(a.secretsHealthResource(), "Secrets: sync, rotation age and usage · ExternalSecrets and SealedSecrets")
	case "mesh", "istio", "linkerd":
		return a.openVirtual(a.meshResource(), "sidecar injection per namespace")
	case "opencost", "spend", "kubecost":
		agg := "namespace"
		if arg == "workload" || arg == "controller" {
			agg = "controller"
		}
		return a.openVirtual(a.costResource(agg), "cost per "+agg+" from OpenCost/Kubecost · :opencost workload for per-workload")
	case "cols", "columns":
		return a.colsCommand(arg)
	case "mouse":
		return a.mouseCommand()
	case "fwd", "fwds":
		return a.fwdCommand(arg + " " + strings.Join(fields[min(2, len(fields)):], " "))
	case "apply":
		return a.openApplyPlan(strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), fields[0])))
	case "files", "ls", "browse":
		if row, ok := a.selected(); ok && a.res.Name == "pods" {
			return a.browseFiles(row.Namespace, row.Name, "", arg)
		}
		if a.files != nil {
			return a.browseFiles(a.files.ns, a.files.pod, a.files.container, nonEmptyStr(arg, a.files.dir))
		}
		a.setStatus("select a pod first, then :files [path] (or press B on it)", true)
		return nil
	case "download", "dl":
		return a.downloadCommand(strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), fields[0])))
	case "upload", "ul":
		return a.uploadCommand(strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), fields[0])))
	case "runbooks", "runbook", "rb":
		return a.runbooksCommand(arg)
	case "autoscaling", "scaling", "autoscalers", "keda", "vpa":
		return a.openVirtual(a.scalingResource(), "HPA, KEDA and VPA · enter: the target workload")
	case "kubeconfig", "kubeconfigs", "kc":
		return a.openVirtual(a.kubeconfigResource(), "enter: open · p: check · r: rename · D: delete · a: add an AKS cluster (az)")
	case "restart-needed", "stale", "staleconfig":
		return a.openVirtual(a.staleResource(), "workloads whose ConfigMap/Secret changed after their pods started · R: rollout restart")
	case "startup", "slowstart":
		if row, ok := a.selected(); ok && a.res.Name == "pods" {
			return a.openStartup(row.Namespace, row.Name)
		}
		a.setStatus("select a pod first, then :startup (or press W on it)", true)
		return nil
	case "find", "search", "grep":
		return a.searchCommand(strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), fields[0])))
	case "policyreports", "kyverno", "gatekeeper", "psa":
		return a.openVirtual(a.policyReportsResource(), "Kyverno, Gatekeeper and Pod Security results · enter: open the object")
	case "quota", "quotas", "capacity":
		return a.openCapacity()
	case "backups", "velero":
		return a.openVirtual(a.backupsResource(), "Velero schedules and backups · enter: describe")
	case "report":
		return a.exportReport(arg)
	case "netcheck", "nettest", "connectivity":
		if row, ok := a.selected(); ok && a.res.Name == "pods" {
			return a.openNetCheck(row.Namespace, row.Name, strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), fields[0])))
		}
		a.setStatus("select a pod first, then :netcheck <target> (or press N on it)", true)
		return nil
	case "prom", "promql", "metrics":
		return a.openPromQuery(strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), fields[0])))
	case "pin", "watch":
		return a.pinCommand(arg, false)
	case "unpin", "unwatch":
		return a.pinCommand(arg, true)
	case "alerts", "watchlist", "notifications":
		return a.openAlerts()
	case "dev":
		return a.devCommand(arg)
	case "clone", "clones", "clone-ns":
		return a.cloneCommand(arg)
	case "plugins", "plugin-catalog", "catalog":
		return a.openVirtual(a.catalogResource(), "plugin catalogue · enter shows the command before installing")
	case "argo-rollouts", "rollouts":
		return a.openVirtual(a.argoRolloutsResource(), "Argo Rollouts · p promote · P promote fully · X abort · T retry")
	case "karpenter":
		return a.openVirtual(a.karpenterResource(), "Karpenter NodePools and NodeClaims")
	case "crossplane", "managed":
		return a.openVirtual(a.crossplaneResource(), "Crossplane managed resources · / filter")
	case "flows", "hubble":
		return a.openHubble()
	case "loki", "logsearch":
		return a.openLoki(arg)
	case "trace", "traces":
		return a.openTrace()
	case "nodepools", "pools", "np":
		return a.openNodePools()
	case "resize":
		return a.resizePod(arg)
	case "team":
		if a.cfg.Team.Repo == "" {
			a.setStatus("no team repository (team.repo in the config)", false)
		} else {
			a.setStatus("team settings from "+a.cfg.Team.Repo+": "+a.cfg.TeamSummary(), false)
		}
		return nil
	case "approvals", "approval":
		return a.openApprovals()
	case "fix", "ai-fix":
		return a.openAIFix()
	case "narrative", "story":
		return a.openNarrative()
	case "nl", "ask-find":
		return a.openNLSearch(arg)
	case "tour", "welcome":
		return a.openTour()
	case "fleet":
		return a.openFleet()
	case "share":
		return a.shareCommand(arg)
	case "daily-report", "summary":
		return a.openDailyReport(arg)
	case "chaos":
		return a.openChaos()
	case "schedules", "schedule":
		return a.openSchedules()
	case "history", "event-history":
		return a.openHistory()
	case "palette":
		return a.openPalette()
	case "timeline", "graph", "rel", "relations":
		if row, ok := a.selected(); ok && graphKinds[a.res.Kind] {
			if cmd == "timeline" {
				return a.openTimeline(a.res.Kind, row.Namespace, row.Name)
			}
			return a.openGraph(a.res.Kind, row.Namespace, row.Name)
		}
		a.setStatus("select an ingress, service, deployment, statefulset, daemonset, replicaset or pod first", true)
		return nil
	case "policy", "policies", "security":
		return a.openPolicy()
	case "resources", "res", "api-resources", "all":
		return a.openNavigator()
	case "clusters", "cluster", "contexts":
		return a.openClusters()
	case "split", "sp", "vsplit", "vs":
		if ctxName, ok := strings.CutPrefix(arg, "@"); ok {
			if ctxName == "" {
				a.setStatus("usage: :vsplit @<context> [namespace]", true)
				return nil
			}
			ns := ""
			if len(fields) > 2 {
				ns = fields[2]
			}
			p := "split"
			if cmd == "split" || cmd == "sp" {
				p = "split-stacked"
			}
			return a.connect(ctxName, ns, p)
		}
		var rp *kube.Resource
		if arg != "" {
			switch arg {
			case "ai", "pulse":
			default:
				r, ok := kube.Lookup(arg)
				if !ok {
					a.setStatus("unknown resource "+arg, true)
					return nil
				}
				rp = &r
			}
		}
		cmd := a.split(cmd == "split" || cmd == "sp", rp)
		switch arg {
		case "ai":
			a.openAI()
		case "pulse":
			return a.openPulse()
		}
		return cmd
	case "close":
		a.closePane()
		return nil
	case "only":
		a.onlyPane()
		return nil
	case "zoom":
		a.zoomed = !a.zoomed && len(a.panes()) > 1
		a.layout()
		return nil
	case "layout":
		return a.applyLayout(arg)
	case "pf", "port-forwards", "forwards":
		return a.openForwards()
	case "ctx", "context":
		if arg != "" {
			return a.connect(arg, "", "tab")
		}
		return a.updateTable(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("c")})
	case "ns!", "namespace!":
		// Set the namespace of every pane at once.
		if arg == "" {
			a.setStatus("usage: :ns! <namespace|all>", true)
			return nil
		}
		var cmds []tea.Cmd
		for _, p := range a.panes() {
			a.withPane(p, func() { cmds = append(cmds, a.setNamespace(arg)) })
		}
		return tea.Batch(cmds...)
	case "ns", "namespace":
		if arg != "" {
			return a.setNamespace(arg)
		}
		return fetchNamespaces(a.client)
	case "pulse", "health", "dash":
		return a.openPulse()
	case "issues", "triage":
		cmd := a.openPulse()
		a.setStatus("enter: analyze issue · t: AI triage of all issues", false)
		return cmd
	case "ai", "chat", "ask":
		a.openAI()
		if q := strings.TrimSpace(strings.TrimPrefix(line, fields[0])); q != "" {
			return a.ask(q)
		}
		return nil
	case "profile", "model":
		if arg != "" {
			a.setProfile(arg)
			return nil
		}
		a.pickProfile()
		return nil
	case "theme":
		if arg != "" {
			a.applyTheme(arg)
			return nil
		}
		a.openPicker("Theme", ThemeNames(), func(s string) tea.Cmd { a.applyTheme(s); return nil })
		return nil
	case "save":
		if err := a.cfg.Save(); err != nil {
			a.setStatus(err.Error(), true)
		} else {
			a.setStatus("config saved to "+a.cfg.Path(), false)
		}
		return nil
	case "help":
		a.ov = ovHelp
		return nil
	}
	if r, ok := a.lookupResource(cmd); ok {
		if arg != "" && r.Namespaced {
			a.client.Namespace = arg
			if arg == "all" || arg == "-A" {
				a.client.Namespace = kube.AllNamespaces
			}
		}
		return a.switchResource(r)
	}
	if _, done := a.discovered[a.client.Context]; !done {
		// Maybe a custom resource (e.g. :certificates, :ingressroutes): discover, then retry.
		a.setStatus("looking up "+cmd+" on "+a.client.Context+"…", false)
		return discover(a.client, "cmd:"+strings.TrimSpace(line))
	}
	a.setStatus("unknown command or resource: "+cmd+"   (0 lists every resource)", true)
	return nil
}
