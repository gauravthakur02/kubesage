// Package ui is the Bubble Tea terminal interface of kubesage.
package ui

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/gauravthakur02/kubesage/internal/agent"
	"github.com/gauravthakur02/kubesage/internal/ai"
	"github.com/gauravthakur02/kubesage/internal/config"
	"github.com/gauravthakur02/kubesage/internal/kube"
	"github.com/gauravthakur02/kubesage/internal/share"
)

type mode int

const (
	modeTable mode = iota
	modeText
	modeAI
	modePulse
)

type overlay int

const (
	ovNone overlay = iota
	ovCommand
	ovFilter
	ovInput
	ovConfirm
	ovPicker
	ovHelp
	ovApprove
	ovTour
)

// Options are command-line overrides.
type Options struct {
	Kubeconfig, Context, Namespace, Resource string
	ReadOnly                                 bool
	// ExtraContexts are opened as additional tabs at startup (--context a,b,c).
	ExtraContexts []string
	// ChooseContext starts with the cluster chooser instead of connecting to the current context.
	ChooseContext bool
	// Version of this build; release builds check GitHub for a newer one at startup.
	Version string
	// Tour shows the first-run tour.
	Tour bool
	// Command is a : command run once at startup (--cmd).
	Command string
}

type targetRef struct {
	res      kube.Resource
	ns, name string
}

type chatEntry struct{ role, display, content string }

type pickerState struct {
	title    string
	items    []string
	cursor   int
	filter   string
	onSelect func(string) tea.Cmd
}

// App is the root model.
type App struct {
	*pane // the focused pane; its fields (res, table, cursor, vp, client…) are promoted

	cfg          *config.Config
	opts         Options
	st           Styles
	w, h         int
	ov           overlay
	version      string
	dangerRe     *regexp.Regexp
	watches      map[*pane]*paneWatch
	newVersion   string              // newer release on GitHub, if any
	writeIntent  bool                // guardWrite passed; the next askConfirm is a change
	sorts        map[string]sortSpec // per resource
	changeReason string              // reason given for the change in progress (audit log)

	// background monitor of the watch list
	alerts        map[string]*alert
	alertOrder    []string
	certAlerts    map[string]alert
	monitorBusy   bool
	monitorPrimed bool
	monitorAt     time.Time
	// read-only web view of the screen (:share)
	sharing  *share.Server
	tourPage int
	// dev loop (:dev)
	dev    *devState
	devGen int
	// two-person rule: changes waiting for approval in this session
	awaitingFn  map[string]func() tea.Cmd
	pendingFn   func() tea.Cmd
	notified    map[string]bool
	approvalsAt time.Time
	runApproved bool
	// AI usage estimate for the session, and the restart floor set by natural-language search
	aiTokIn, aiTokOut int
	minRestarts       int
	// local event history
	evStore     *kube.EventStore
	evAt        time.Time
	evBusy      bool
	evPruned    map[string]bool
	certCheckAt time.Time
	monitorErr  string

	// launcher: the startup cluster chooser is showing (no cluster contacted yet)
	launcher     bool
	launchCursor bool

	// workspace tabs; the live tab's layout is in root/pane/zoomed
	tabs []*workspace
	tab  int

	// split layout
	root          *layoutNode
	nextID        int
	zoomed        bool
	renderFocused bool

	// AI
	provider    ai.Provider
	providerErr error
	profile     string
	chat        []chatEntry
	aiVP        viewport.Model
	aiInput     textinput.Model
	aiBusy      bool
	aiCh        chan string
	aiErrCh     chan error
	aiCancel    context.CancelFunc
	aiGen       int
	spin        spinner.Model

	// clusters: shared connection pool and health
	cl *clusterStore
	// discovered custom resources / other APIs per context
	discovered map[string][]kube.Resource
	// aiClient is the cluster the current AI conversation is about; AI commands run there.
	aiClient *kube.Client

	// overlays
	input     textinput.Model
	inputHint string
	onInput   func(string) tea.Cmd
	confirm   string
	onConfirm func() tea.Cmd
	picker    pickerState

	// AI command execution & port-forwards
	ag         agentState
	pending    *targetRef                          // row to select once the next list arrives (forward "go to")
	execFn     func(agent.Command) (string, error) // overridable in tests
	pfReported map[int]bool

	logoTicks int // drives the logo animation (advanced by the spinner tick)
	hist      *metricHistory

	status    string
	statusErr bool
}

// New builds the application model.
func New(cfg *config.Config, client *kube.Client, opts Options) *App {
	a := &App{cfg: cfg, opts: opts, st: newStyles(themeByName(cfg.Theme)), pfReported: map[int]bool{}, renderFocused: true}
	r, _ := kube.Lookup("pods")
	if opts.Resource != "" {
		if rr, ok := kube.Lookup(opts.Resource); ok {
			r = rr
		}
	}
	a.nextID = 1
	a.cl = newClusterStore()
	a.discovered = map[string][]kube.Resource{}
	a.hist = newHistory()
	a.cl.add(client)
	a.pane = newPane(1, client.WithNamespace(client.Namespace), r)
	a.root = &layoutNode{p: a.pane}
	a.tabs = []*workspace{{root: a.root, focus: a.pane}}
	if re, err := regexp.Compile(cfg.DangerContexts); err == nil && cfg.DangerContexts != "" {
		a.dangerRe = re
	}
	a.spin = spinner.New(spinner.WithSpinner(spinner.MiniDot))
	a.input = textinput.New()
	a.input.Prompt = ""
	a.aiInput = textinput.New()
	a.aiInput.Prompt = "› "
	a.aiInput.Placeholder = "Ask about your cluster… (enter to send, esc to go back)"
	a.aiInput.CharLimit = 4000
	a.aiVP = viewport.New(80, 20)
	a.restyle()
	a.setProfile(cfg.AI.Active)
	return a
}

func (a *App) restyle() {
	a.spin.Style = a.st.Title
	a.aiInput.PromptStyle = a.st.Title
	a.aiInput.TextStyle = a.st.Base
	a.aiInput.PlaceholderStyle = a.st.Muted
	a.input.TextStyle = a.st.Base
}

func (a *App) setProfile(name string) {
	a.profile = name
	p, ok := a.cfg.AI.Profiles[name]
	if !ok {
		a.provider, a.providerErr = nil, fmt.Errorf("AI profile %q not found in %s", name, a.cfg.Path())
		return
	}
	a.provider, a.providerErr = ai.New(name, p)
}

// pickProfile lists AI profiles with provider, model and whether their key was found.
func (a *App) pickProfile() {
	names := a.cfg.ProfileNames()
	labels := make([]string, len(names))
	for i, n := range names {
		p := a.cfg.AI.Profiles[n]
		model := p.Model
		if model == "" {
			model = "auto"
		}
		cur := "  "
		if n == a.profile {
			cur = "★ "
		}
		labels[i] = fmt.Sprintf("%s%-10s %-10s %-24s %s", cur, n, p.Provider, model, p.KeyStatus())
	}
	const addLabel, keyLabel = "+ Add AI profile…", "⚿ Set API key for the active profile…"
	labels = append(labels, addLabel, keyLabel)
	a.openPicker("AI profile  (★ active · enter switch · keys are stored in "+config.KeyringName()+")", labels, func(s string) tea.Cmd {
		switch s {
		case addLabel:
			return a.addProfileWizard()
		case keyLabel:
			return a.promptKey(a.profile)
		}
		for i, l := range labels {
			if l == s {
				a.setProfile(names[i])
				if p := a.cfg.AI.Profiles[names[i]]; p.NeedsKey() && p.Key() == "" {
					return a.promptKey(names[i]) // switching to a profile without a key: ask for it now
				}
				if a.providerErr != nil {
					a.setStatus(a.providerErr.Error(), true)
				} else {
					a.setStatus("AI → "+names[i]+" · "+a.provider.Model()+" · "+a.cfg.AI.Profiles[names[i]].KeyStatus(), false)
				}
				a.renderChat()
			}
		}
		return nil
	})
}

// logoFrame is the current animation frame, or -1 when animations are off.
func (a *App) logoFrame() int {
	if !a.cfg.Animations {
		return -1
	}
	return a.logoTicks / 2
}

func (a *App) readOnly() bool { return a.opts.ReadOnly || a.cfg.ReadOnly }
func (a *App) danger() bool   { return a.dangerFor(a.client.Context) }

// Init starts the first fetches and timers.
func (a *App) Init() tea.Cmd {
	a.loading = true
	if a.opts.Tour {
		a.openTour()
	}
	if a.opts.ChooseContext {
		a.launchCursor = true
		return tea.Batch(a.openLauncher(), tick(a.refreshEvery()), a.spin.Tick, a.mouseInit())
	}
	cmds := []tea.Cmd{tagPane(a.pane, fetchList(a.client, a.res, a.listGen)), fetchVersion(a.client), tick(a.refreshEvery()), a.spin.Tick, a.checkUpdate(), a.mouseInit()}
	for _, ctx := range a.opts.ExtraContexts {
		cmds = append(cmds, a.connect(ctx, a.opts.Namespace, "bgtab"))
	}
	if a.opts.Command != "" {
		cmds = append(cmds, a.runCommand(a.opts.Command))
	}
	return tea.Batch(cmds...)
}

func (a *App) refreshEvery() time.Duration { return time.Duration(a.cfg.RefreshSeconds) * time.Second }

// slowRefreshEvery is the refresh interval of report tables that query the cluster heavily.
func (a *App) slowRefreshEvery() time.Duration {
	if a.cfg.SlowRefreshSeconds <= 0 {
		return 30 * time.Second
	}
	return time.Duration(a.cfg.SlowRefreshSeconds) * time.Second
}

func (a *App) setStatus(s string, isErr bool) { a.status, a.statusErr = s, isErr }

// ---------- selection helpers ----------

func (a *App) applyFilter() {
	a.visible = a.visible[:0]
	f := strings.ToLower(a.filter)
	words := strings.Fields(f)
	for i, r := range a.table.Rows {
		if a.minRestarts > 0 && a.rowRestarts(r) < a.minRestarts {
			continue
		}
		line := strings.ToLower(strings.Join(r.Cells, " "))
		match := f == "" || strings.Contains(line, f)
		if !match && len(words) > 1 { // several words: every word must appear (natural-language search)
			match = true
			for _, w := range words {
				if !strings.Contains(line, w) {
					match = false
					break
				}
			}
		}
		if match {
			a.visible = append(a.visible, i)
		}
	}
	a.applySort()
	if a.cursor >= len(a.visible) {
		a.cursor = max(0, len(a.visible)-1)
	}
}

func (a *App) selected() (kube.Row, bool) {
	if a.cursor < 0 || a.cursor >= len(a.visible) {
		return kube.Row{}, false
	}
	return a.table.Rows[a.visible[a.cursor]], true
}

func (a *App) switchResource(r kube.Resource) tea.Cmd {
	a.stopLogs()
	a.res, a.mode, a.cursor, a.offset, a.filter, a.minRestarts = r, modeTable, 0, 0, "", 0
	a.table = kube.Table{}
	a.visible = nil
	return a.refresh()
}

func (a *App) refresh() tea.Cmd {
	a.listGen++
	a.loading = true
	a.lastList = time.Now()
	if cols := a.customColumns(a.res); len(cols) > 0 && a.res.Watchable() {
		return tagPane(a.pane, fetchListColumns(a.client, a.res, a.listGen, cols))
	}
	return tagPane(a.pane, fetchList(a.client, a.res, a.listGen))
}

func (a *App) setNamespace(ns string) tea.Cmd {
	if ns == "all" {
		ns = kube.AllNamespaces
	}
	a.client.Namespace = ns
	a.setStatus("namespace → "+a.client.NamespaceLabel(), false)
	a.pulse, a.issues = nil, nil
	if a.mode == modePulse {
		a.pulseBusy = true
		return tagPane(a.pane, fetchPulse(a.client))
	}
	return a.switchResource(a.res)
}

// ---------- Update ----------

// Update handles every message, routing pane-scoped results to their pane.
func (a *App) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if k, ok := msg.(tea.KeyMsg); ok {
		msg = a.remapKey(k)
	}
	if m, ok := msg.(tea.MouseMsg); ok {
		return a, a.handleMouse(m)
	}
	if pm, ok := msg.(paneMsg); ok {
		if p := pm.targetPane(); p != nil && p != a.pane {
			if !a.hasPane(p) {
				return a, nil // pane was closed
			}
			var cmd tea.Cmd
			a.withPane(p, func() { _, cmd = a.update(msg) })
			return a, cmd
		}
	}
	return a.update(msg)
}

func (a *App) update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		a.w, a.h = msg.Width, msg.Height
		a.layout()
		return a, nil

	case spinner.TickMsg:
		var cmd tea.Cmd
		a.spin, cmd = a.spin.Update(msg)
		a.logoTicks++
		if a.aiBusy || (a.cfg.Animations && len(a.chat) == 0 && a.findPaneInMode(modeAI) != nil && a.logoTicks%2 == 0) {
			a.renderChat() // the empty chat shows the animated logo
		}
		return a, cmd

	case tickMsg:
		a.checkForwards()
		cmds := append([]tea.Cmd{tick(a.refreshEvery())}, a.autoMask()...)
		cmds = append(cmds, a.ensureWatches()...)
		if cmd := a.pollApprovals(); cmd != nil {
			cmds = append(cmds, cmd)
		}
		if cmd := a.recordEvents(); cmd != nil {
			cmds = append(cmds, cmd)
		}
		if len(a.cfg.Watchlist) > 0 && time.Since(a.monitorAt) >= a.alertInterval() {
			cmds = append(cmds, a.runMonitor())
		}
		for _, p := range a.panes() {
			switch {
			case p.mode == modeText && p.textKind == "health" && (!a.zoomed || p == a.pane):
				cmds = append(cmds, tagPane(p, fetchHealth(p.client, p.target.ns, p.target.name, true)))
			case p.mode == modeText && p.textKind == "nodehealth" && (!a.zoomed || p == a.pane):
				cmds = append(cmds, tagPane(p, fetchNodeHealth(p.client, p.target.name, true)))
			case p.mode == modeText && p.textKind == "secret" && p.reveal:
				a.withPane(p, func() { a.renderSecret(false) }) // countdown
			}
		}
		if a.ov != ovNone {
			return a, tea.Batch(cmds...)
		}
		for _, p := range a.panes() {
			if a.zoomed && p != a.pane {
				continue
			}
			switch {
			case p.mode == modeTable && !p.loading && a.watching(p) && time.Since(p.lastList) < watchResync:
				// live watch keeps this pane current
			case p.mode == modeTable && !p.loading && p.res.Slow && time.Since(p.lastList) < a.slowRefreshEvery():
				// heavy report table: refreshed less often (r refreshes now)
			case p.mode == modeTable && !p.loading:
				cmds = append(cmds, a.refreshPane(p))
			case p.mode == modePulse && !p.pulseBusy && time.Now().Second()%15 < a.cfg.RefreshSeconds:
				p.pulseBusy = true
				cmds = append(cmds, tagPane(p, fetchPulse(p.client)))
			}
		}
		return a, tea.Batch(cmds...)

	case listMsg:
		if msg.gen != a.listGen {
			return a, nil
		}
		a.loading = false
		a.listErr = msg.err
		if msg.err == nil {
			a.table = msg.table
			if a.res.Name == "pods" {
				a.recordTable(a.client.Context, msg.table)
			}
			a.applyFilter()
			a.applyPendingSelect()
			if a.launchCursor && a.res.Name == clustersRes { // start on the kubeconfig current-context
				a.launchCursor = false
				for i, idx := range a.visible {
					if a.table.Rows[idx].Name == a.client.Context {
						a.cursor = i
					}
				}
			}
		}
		return a, nil

	case versionMsg:
		a.cl.update(msg.ctx, func(i *clusterInfo) {
			if msg.err == nil {
				i.version, i.err = msg.v, nil
			} else {
				i.err = msg.err
			}
			i.at = time.Now()
		})
		return a, nil

	case connectMsg:
		cmd := a.handleConnect(msg)
		a.syncUsage()
		return a, cmd

	case probeMsg:
		var cmds []tea.Cmd
		for _, p := range a.panes() {
			if p.res.Name == clustersRes && p.mode == modeTable {
				cmds = append(cmds, a.refreshPane(p))
			}
		}
		return a, tea.Batch(cmds...)

	case textMsg:
		if msg.err != nil {
			a.setStatus(msg.err.Error(), true)
			a.mode = a.prevMode
			return a, nil
		}
		a.textTitle, a.textKind = msg.title, msg.kind
		a.textLines = strings.Split(strings.TrimRight(msg.content, "\n"), "\n")
		a.renderText(true)
		return a, nil

	case logLinesMsg:
		if msg.gen != a.logGen {
			return a, nil
		}
		a.textLines = append(a.textLines, msg.lines...)
		if len(a.textLines) > 10000 {
			a.textLines = a.textLines[len(a.textLines)-10000:]
		}
		a.renderText(a.follow)
		if msg.done {
			a.textLines = append(a.textLines, "── stream closed ──")
			a.renderText(a.follow)
			return a, nil
		}
		return a, tagPane(a.pane, waitLines(a.logCh, a.logGen))

	case aiPromptMsg:
		if msg.err != nil {
			a.aiBusy = false
			a.chat = append(a.chat, chatEntry{role: "error", display: msg.err.Error()})
			a.renderChat()
			return a, nil
		}
		prompt, n := a.withRunbooks(msg.prompt)
		display := msg.display
		if n > 0 {
			display += fmt.Sprintf(" · with %d team runbook(s)", n)
		}
		return a, a.sendToAI(display, prompt)

	case aiChunkMsg:
		if msg.gen != a.aiGen || len(a.chat) == 0 {
			return a, nil
		}
		last := &a.chat[len(a.chat)-1]
		last.content += msg.text
		if msg.done {
			a.aiBusy = false
			a.countTokens("", last.content)
			if msg.err != nil && msg.err != context.Canceled {
				a.chat = append(a.chat, chatEntry{role: "error", display: msg.err.Error()})
				a.renderChat()
				return a, nil
			}
			a.renderChat()
			return a, a.afterReply(last.content)
		}
		a.renderChat()
		return a, waitAI(a.aiCh, a.aiErrCh, a.aiGen)

	case pulseMsg:
		a.pulseBusy = false
		a.pulseErr = msg.err
		if msg.err == nil {
			a.pulse, a.issues = &msg.pulse, msg.issues
			if a.issueCursor >= len(a.issues) {
				a.issueCursor = max(0, len(a.issues)-1)
			}
		}
		return a, nil

	case actionMsg:
		a.audit(msg.status, a.client.Context, a.client.Namespace, "", msg.err)
		if msg.err != nil {
			a.setStatus(msg.status+" failed: "+msg.err.Error(), true)
		} else {
			a.setStatus("✓ "+msg.status, false)
		}
		return a, a.refresh()

	case execDoneMsg:
		if msg.err != nil {
			a.setStatus(msg.err.Error(), true)
		}
		return a, a.refresh()

	case namespacesMsg:
		if msg.err != nil {
			a.setStatus(msg.err.Error(), true)
			return a, nil
		}
		a.openPicker("Namespace", msg.names, a.setNamespace)
		return a, nil

	case podsMsg:
		return a, a.handlePods(msg)

	case shellDoneMsg:
		return a, a.handleShellDone(msg)

	case applyPlanMsg:
		a.handleApplyPlan(msg)
		return a, nil
	case dirMsg:
		return a, a.handleDir(msg)
	case fileMsg:
		a.handleFile(msg)
		return a, nil
	case aksSubsMsg:
		return a, a.handleAKSSubs(msg)
	case aksClustersMsg:
		return a, a.handleAKSClusters(msg)
	case kubeconfigChangedMsg:
		return a, a.handleKubeconfigChanged(msg)

	case approvalRequestedMsg:
		return a, a.handleApprovalRequested(msg)
	case approvalsPolledMsg:
		return a, a.handleApprovalsPolled(msg)
	case devTickMsg:
		return a, a.handleDevTick(msg)
	case devAppliedMsg:
		return a, a.handleDevApplied(msg)
	case pluginChosenMsg:
		return a, a.handlePluginChosen(msg)
	case poolScaledMsg:
		return a, a.handlePoolScaled(msg)
	case resizeCheckedMsg:
		return a, a.handleResizeChecked(msg)
	case aiFixMsg:
		return a, a.handleAIFix(msg)
	case nlSearchMsg:
		return a, a.handleNLSearch(msg)

	case eventsRecordedMsg:
		return a, a.handleEventsRecorded(msg)

	case alertsMsg:
		a.handleAlerts(msg)
		if a.res.Name == alertsRes && a.mode == modeTable {
			return a, a.refresh()
		}
		return a, nil

	case updateMsg:
		a.newVersion = msg.version
		a.setStatus(fmt.Sprintf("⬆ kubesage %s is available (you have %s): %s", msg.version, a.opts.Version, msg.url), false)
		return a, nil

	case watchEventMsg:
		return a, a.handleWatchEvent(msg)

	case watchEndMsg:
		return a, a.handleWatchEnd(msg)

	case helmTextMsg:
		a.handleHelmText(msg)
		return a, nil

	case nodeHealthMsg:
		a.handleNodeHealth(msg)
		return a, nil

	case drainPlanMsg:
		a.handleDrainPlan(msg)
		return a, nil

	case drainDoneMsg:
		return a, tea.Batch(a.handleDrainDone(msg), a.openDrainPreview(a.target.name))

	case editLoadedMsg:
		return a, a.handleEditLoaded(msg)

	case editDoneMsg:
		return a, a.handleEditDone(msg)

	case dryRunMsg:
		a.handleDryRun(msg)
		return a, nil

	case applyDoneMsg:
		return a, a.handleApplyDone(msg)

	case shellAllowedMsg:
		return a, a.handleShellAllowed(msg)

	case viewReadyMsg:
		return a, a.handleViewReady(msg)

	case discoverMsg:
		return a, a.handleDiscover(msg)

	case healthMsg:
		a.handleHealth(msg)
		return a, nil

	case secretMsg:
		a.handleSecret(msg)
		return a, nil

	case cmdResultMsg:
		return a, a.handleCmdResult(msg)

	case pfTargetsMsg:
		return a, a.handlePortTargets(msg)

	case pfStartedMsg:
		if msg.err != nil {
			a.setStatus("port-forward failed: "+msg.err.Error(), true)
		} else {
			status := "⇄ forwarding " + msg.fw.URL() + " → " + strings.TrimPrefix(msg.fw.String(), fmt.Sprintf("localhost:%d → ", msg.fw.Local))
			if a.cfg.OpenBrowser {
				if err := openBrowserFn(msg.fw.URL()); err != nil {
					status += "  (could not open browser: " + err.Error() + ")"
				} else {
					status += "  · opened in browser"
				}
			}
			a.setStatus(status+"   (:pf to manage)", false)
		}
		return a, nil
	}

	if key, ok := msg.(tea.KeyMsg); ok {
		if key.String() == "ctrl+c" {
			a.quitCleanup()
			return a, tea.Quit
		}
		if a.ov != ovNone {
			return a, a.updateOverlay(key)
		}
		if cmd, handled := a.paneKeys(key); handled {
			return a, cmd
		}
		switch a.mode {
		case modeText:
			return a, a.updateText(key)
		case modeAI:
			return a, a.updateAI(key)
		case modePulse:
			return a, a.updatePulse(key)
		default:
			return a, a.updateTable(key)
		}
	}
	return a, nil
}

// layout assigns sizes to every pane from the split tree (or the zoomed pane).
func (a *App) layout() {
	areaW, areaH := max(24, a.w), max(5, a.h-4)
	if a.zoomed {
		a.root.assign(areaW, areaH) // keep others sized for when zoom ends
		a.root.find(a.pane).assign(areaW, areaH)
	} else {
		a.root.assign(areaW, areaH)
	}
	for _, p := range a.panes() {
		a.withPane(p, func() {
			a.vp.Width, a.vp.Height = a.pw, a.ph
			a.renderText(false)
		})
	}
	if p := a.findPaneInMode(modeAI); p != nil {
		a.withPane(p, a.sizeChat)
	}
	a.renderChat()
}

// bodySize is the drawable area inside the focused pane.
func (a *App) bodySize() (int, int) { return a.pw, a.ph }

// fullSize is the area available to modals (all panes together).
func (a *App) fullSize() (int, int) { return max(20, a.w-2), max(3, a.h-6) }

func (a *App) quitCleanup() {
	for _, p := range a.allPanes() {
		a.withPane(p, a.stopLogs)
	}
	a.stopAI()
	kube.StopAllForwards()
}

// ---------- table mode ----------

func nsOrDefault(ns string) string {
	if ns == "" {
		return "default"
	}
	return ns
}

func (a *App) guardWrite() bool {
	if a.readOnly() {
		a.setStatus("read-only mode: mutating actions are disabled", true)
		return false
	}
	if msg, warn := changePolicy(a.cfg, a.client.Context, time.Now()); msg != "" {
		if !warn {
			a.setStatus("⛔ "+msg, true)
			return false
		}
		a.setStatus("⚠ "+msg+" (warning only)", true)
	}
	a.writeIntent = true // the next confirmation is for a change (may need a reason)
	return true
}

func (a *App) applyTheme(name string) {
	a.st = newStyles(themeByName(name))
	a.cfg.Theme = name
	a.restyle()
	a.renderText(false)
	a.renderChat()
	a.setStatus("theme → "+name, false)
}

func (a *App) handlePods(msg podsMsg) tea.Cmd {
	if msg.err != nil {
		a.setStatus(msg.err.Error(), true)
		return nil
	}
	if len(msg.pods) == 0 {
		a.setStatus("no pods found", true)
		return nil
	}
	pickContainer := func(idx int) tea.Cmd {
		p := msg.pods[idx]
		if msg.purpose == "health" {
			return a.openHealth(p.Namespace, p.Name)
		}
		ctrs := kube.Containers(&p)
		if msg.purpose == "shell" || msg.purpose == "debug" { // init containers have exited; only running containers
			ctrs = nil
			for _, c := range p.Spec.Containers {
				ctrs = append(ctrs, c.Name)
			}
		}
		run := func(c string) tea.Cmd {
			switch msg.purpose {
			case "shell":
				return a.openShell(p.Namespace, p.Name, c)
			case "debug":
				target := p
				a.askConfirm(fmt.Sprintf("Attach an ephemeral debug container (%s) to %s [%s]? This permanently adds it to the pod spec.",
					a.debugImage(), target.Name, c), "", func() tea.Cmd { return a.debugShell(target.Namespace, target.Name, c) })
				return nil
			default:
				return a.openLogs(p.Namespace, p.Name, c, msg.purpose == "prevlogs")
			}
		}
		if len(ctrs) == 1 {
			return run(ctrs[0])
		}
		a.openPicker("Container in "+p.Name, ctrs, run)
		return nil
	}
	if len(msg.pods) == 1 {
		return pickContainer(0)
	}
	var names []string
	merged := fmt.Sprintf("★ all %d pods, merged", len(msg.pods))
	if msg.purpose == "logs" || msg.purpose == "prevlogs" {
		names = append(names, merged)
	}
	for i := range msg.pods {
		names = append(names, msg.pods[i].Name+"  "+kube.PodStatus(&msg.pods[i]))
	}
	a.openPicker("Pod", names, func(s string) tea.Cmd {
		if s == merged {
			return a.openMergedLogs(msg.pods)
		}
		for i, n := range names {
			if n == s {
				return pickContainer(i)
			}
		}
		return nil
	})
	return nil
}

// ---------- commands ----------

// ---------- text / logs view ----------

func (a *App) openText(r kube.Resource, ns, name, kind string) tea.Cmd {
	a.stopLogs()
	a.prevMode, a.mode = a.mode, modeText
	a.target = targetRef{r, ns, name}
	a.textTitle, a.textKind = "loading "+kind+"…", kind
	a.textLines = nil
	a.renderText(true)
	return tagPane(a.pane, fetchText(a.client, r, ns, name, kind, a.reveal))
}

func (a *App) openLogs(ns, pod, container string, previous bool) tea.Cmd {
	a.stopLogs()
	if a.mode != modeText {
		a.prevMode = a.mode
	}
	return a.startLogs(logSpec{ns: ns, pods: []string{pod}, container: container, previous: previous})
}

func (a *App) stopLogs() {
	if a.logCancel != nil {
		a.logCancel()
		a.logCancel = nil
	}
	a.logGen++
}

// ---------- pulse / issues ----------

func (a *App) openPulse() tea.Cmd {
	if p := a.findPaneInMode(modePulse); p != nil && p != a.pane &&
		p.client.Context == a.client.Context && p.client.Namespace == a.client.Namespace {
		a.focus(p)
		a.pulseBusy = true
		return tagPane(a.pane, fetchPulse(a.client))
	}
	a.stopLogs()
	a.mode = modePulse
	a.pulseBusy = true
	return tagPane(a.pane, fetchPulse(a.client))
}

// ---------- AI ----------

func (a *App) openAI() {
	if p := a.findPaneInMode(modeAI); p != nil && p != a.pane {
		a.focus(p) // reuse the AI pane that is already on screen
		a.renderChat()
		return
	}
	if a.mode != modeAI {
		a.prevMode = a.mode
	}
	a.mode = modeAI
	a.aiInput.Focus()
	a.sizeChat()
	a.renderChat()
}

func (a *App) analyze(r kube.Resource, ns, name string) tea.Cmd {
	target := a.client // the cluster of the row being analysed, before focus moves to the AI pane
	a.openAI()
	if a.provider == nil {
		a.chat = append(a.chat, chatEntry{role: "error", display: fmt.Sprint(a.providerErr)})
		a.renderChat()
		return nil
	}
	a.stopAI()
	a.ag = agentState{}
	a.aiClient = target
	a.aiBusy = true
	where := ""
	if a.multiCluster() {
		where = " on " + target.Context
	}
	a.chat = append(a.chat, chatEntry{role: "user", display: fmt.Sprintf("Collecting manifest, events and logs for %s %s%s…", strings.ToLower(r.Kind), objName(ns, name), where)})
	a.renderChat()
	return gatherForAI(target, r, ns, name, a.cfg.AI.Redact, a.cfg.AI.LogLines)
}

// ask sends a free-form question; the first one in a conversation gets cluster context.
func (a *App) ask(q string) tea.Cmd {
	if a.provider == nil {
		a.chat = append(a.chat, chatEntry{role: "error", display: fmt.Sprint(a.providerErr)})
		a.renderChat()
		return nil
	}
	hasHistory := false
	for _, c := range a.chat {
		if c.role == "assistant" {
			hasHistory = true
		}
	}
	if !hasHistory {
		a.aiClient = a.client
		a.aiBusy = true
		a.chat = append(a.chat, chatEntry{role: "user", display: q + "  (attaching cluster context…)"})
		a.renderChat()
		return chatPrelude(a.client, q)
	}
	return a.sendToAI(q, q)
}

// sendToAI appends the user turn and starts streaming the reply.
func (a *App) sendToAI(display, prompt string) tea.Cmd {
	kctx := a.client.Context
	if a.aiClient != nil {
		kctx = a.aiClient.Context
	}
	if why := a.aiBlocked(kctx); why != "" {
		a.aiBusy = false
		a.chat = append(a.chat, chatEntry{role: "error", display: why})
		a.renderChat()
		return nil
	}
	// Replace a pending placeholder ("collecting…") with the real user turn.
	if n := len(a.chat); n > 0 && a.chat[n-1].role == "user" && a.chat[n-1].content == "" {
		a.chat = a.chat[:n-1]
	}
	a.chat = append(a.chat, chatEntry{role: "user", display: display, content: prompt})
	var msgs []ai.Message
	for _, c := range a.chat {
		if (c.role == "user" || c.role == "assistant") && c.content != "" {
			msgs = append(msgs, ai.Message{Role: c.role, Content: c.content})
		}
	}
	a.chat = append(a.chat, chatEntry{role: "assistant"})
	a.aiBusy = true
	a.aiGen++
	for _, m := range msgs {
		a.countTokens(m.Content, "")
	}
	a.aiCh, a.aiErrCh, a.aiCancel = startAIStream(a.provider, a.systemPrompt(), msgs)
	a.renderChat()
	return waitAI(a.aiCh, a.aiErrCh, a.aiGen)
}

func (a *App) stopAI() {
	if a.aiCancel != nil {
		a.aiCancel()
		a.aiCancel = nil
	}
	a.aiGen++
	a.aiBusy = false
	a.ag.running = false
	a.ag.queue, a.ag.results, a.ag.idx = nil, nil, 0
}

// ---------- overlays ----------

func (a *App) openInput(kind overlay, hint string, onSubmit func(string) tea.Cmd) {
	a.ov = kind
	a.inputHint = hint
	a.onInput = onSubmit
	a.input.SetValue("")
	a.input.Focus()
}

func (a *App) askConfirm(question, typeToConfirm string, fn func() tea.Cmd) {
	if a.writeIntent && a.needsApproval() {
		a.writeIntent = false
		a.requestApproval(question, fn)
		return
	}
	if a.writeIntent && a.reasonRequired() {
		fn = a.withReason(fn)
	}
	a.writeIntent = false
	a.ov = ovConfirm
	a.confirm = question
	a.onConfirm = fn
	a.input.SetValue("")
	a.inputHint = ""
	// On production-like contexts require typing the object name.
	if a.danger() && typeToConfirm != "" {
		a.inputHint = typeToConfirm
		a.input.Focus()
	}
}

func (a *App) openPicker(title string, items []string, onSelect func(string) tea.Cmd) {
	a.ov = ovPicker
	a.picker = pickerState{title: title, items: items, onSelect: onSelect}
}

func (p *pickerState) visibleItems() []string {
	if p.filter == "" {
		return p.items
	}
	// Rank: exact ":alias" or a word starting with the filter first, then substring matches.
	f := strings.ToLower(p.filter)
	var best, good, rest []string
	for _, it := range p.items {
		l := strings.ToLower(it)
		switch {
		case !strings.Contains(l, f):
			continue
		case strings.HasSuffix(l, ":"+f) || strings.Contains(l, ":"+f+" "):
			best = append(best, it)
		case strings.HasPrefix(strings.TrimLeft(l, "▸★ "), f) || strings.Contains(l, " "+f):
			good = append(good, it)
		default:
			rest = append(rest, it)
		}
	}
	return append(append(best, good...), rest...)
}
