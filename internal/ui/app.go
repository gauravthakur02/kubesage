// Package ui is the Bubble Tea terminal interface of kubesage.
package ui

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"

	"kubesage/internal/agent"
	"kubesage/internal/ai"
	"kubesage/internal/config"
	"kubesage/internal/kube"
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
)

// Options are command-line overrides.
type Options struct {
	Kubeconfig, Context, Namespace, Resource string
	ReadOnly                                 bool
	// ExtraContexts are opened as additional tabs at startup (--context a,b,c).
	ExtraContexts []string
	// ChooseContext starts with the cluster chooser instead of connecting to the current context.
	ChooseContext bool
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

	cfg      *config.Config
	opts     Options
	st       Styles
	w, h     int
	ov       overlay
	version  string
	dangerRe *regexp.Regexp
	watches  map[*pane]*paneWatch

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
	if a.opts.ChooseContext {
		a.launchCursor = true
		return tea.Batch(a.openLauncher(), tick(a.refreshEvery()), a.spin.Tick)
	}
	cmds := []tea.Cmd{tagPane(a.pane, fetchList(a.client, a.res, a.listGen)), fetchVersion(a.client), tick(a.refreshEvery()), a.spin.Tick}
	for _, ctx := range a.opts.ExtraContexts {
		cmds = append(cmds, a.connect(ctx, a.opts.Namespace, "bgtab"))
	}
	return tea.Batch(cmds...)
}

func (a *App) refreshEvery() time.Duration { return time.Duration(a.cfg.RefreshSeconds) * time.Second }

func (a *App) setStatus(s string, isErr bool) { a.status, a.statusErr = s, isErr }

// ---------- selection helpers ----------

func (a *App) applyFilter() {
	a.visible = a.visible[:0]
	f := strings.ToLower(a.filter)
	for i, r := range a.table.Rows {
		if f == "" || strings.Contains(strings.ToLower(strings.Join(r.Cells, " ")), f) {
			a.visible = append(a.visible, i)
		}
	}
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
	a.res, a.mode, a.cursor, a.offset, a.filter = r, modeTable, 0, 0, ""
	a.table = kube.Table{}
	a.visible = nil
	return a.refresh()
}

func (a *App) refresh() tea.Cmd {
	a.listGen++
	a.loading = true
	a.lastList = time.Now()
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
		return a, a.sendToAI(msg.display, msg.prompt)

	case aiChunkMsg:
		if msg.gen != a.aiGen || len(a.chat) == 0 {
			return a, nil
		}
		last := &a.chat[len(a.chat)-1]
		last.content += msg.text
		if msg.done {
			a.aiBusy = false
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

// paneKeys handles split/focus/zoom keys. Typing keys are left alone in the AI chat.
func (a *App) paneKeys(k tea.KeyMsg) (tea.Cmd, bool) {
	switch k.String() {
	case "ctrl+w":
		a.focusNext(1)
		return nil, true
	case "ctrl+x":
		if len(a.panes()) == 1 && len(a.tabs) > 1 {
			return a.closeTab(), true
		}
		a.closePane()
		return nil, true
	case "ctrl+t":
		return a.newTab(""), true
	case "alt+1", "alt+2", "alt+3", "alt+4", "alt+5", "alt+6":
		if ps := a.panes(); int(k.String()[4]-'1') < len(ps) {
			a.focus(ps[k.String()[4]-'1'])
		}
		return nil, true
	}
	if a.mode == modeAI {
		return nil, false
	}
	switch k.String() {
	case "|", "\\":
		return a.split(false, nil), true
	case "-", "_":
		return a.split(true, nil), true
	case ">", ".":
		if len(a.tabs) > 1 {
			return a.switchTab((a.tab + 1) % len(a.tabs)), true
		}
		return nil, true
	case "<", ",":
		if len(a.tabs) > 1 {
			return a.switchTab((a.tab - 1 + len(a.tabs)) % len(a.tabs)), true
		}
		return nil, true
	case "z":
		if len(a.panes()) > 1 {
			a.zoomed = !a.zoomed
			a.layout()
		}
		return nil, true
	case "[":
		a.resize(-0.05)
		return nil, true
	case "]":
		a.resize(0.05)
		return nil, true
	}
	return nil, false
}

// ---------- table mode ----------

func (a *App) updateTable(k tea.KeyMsg) tea.Cmd {
	_, innerH := a.bodySize()
	page := max(1, innerH-1)
	if a.res.Name == clustersRes {
		if row, ok := a.selected(); ok {
			if cmd, handled := a.clusterRowAction(k.String(), row); handled {
				return cmd
			}
		}
	}
	if _, _, ok := isRollout(a.res); ok {
		if row, ok := a.selected(); ok {
			if cmd, handled := a.rolloutRowAction(k.String(), row); handled {
				return cmd
			}
		}
	}
	if a.res.Name == policyRes {
		if row, ok := a.selected(); ok {
			if cmd, handled := a.policyRowAction(k.String(), row); handled {
				return cmd
			}
		}
	}
	if a.res.Name == helmRes {
		if row, ok := a.selected(); ok {
			if cmd, handled := a.helmRowAction(k.String(), row); handled {
				return cmd
			}
		}
	}
	if _, _, ok := isHelmHistory(a.res); ok {
		if row, ok := a.selected(); ok {
			if cmd, handled := a.helmHistoryRowAction(k.String(), row); handled {
				return cmd
			}
		}
	}
	if k.String() == "esc" && a.filter == "" && a.backTo != nil {
		back := *a.backTo
		a.backTo = nil
		return a.switchResource(back)
	}
	switch k.String() {
	case "q":
		a.quitCleanup()
		return tea.Quit
	case "!":
		a.openInput(ovInput, "kubectl ", a.runManual)
		return nil
	case "f":
		return a.openForwards()
	case "esc":
		if a.filter != "" {
			a.filter = ""
			a.applyFilter()
		}
	case "up", "k":
		a.cursor = max(0, a.cursor-1)
	case "down", "j":
		a.cursor = min(len(a.visible)-1, a.cursor+1)
	case "pgup", "ctrl+u":
		a.cursor = max(0, a.cursor-page)
	case "pgdown", "ctrl+f":
		a.cursor = min(len(a.visible)-1, a.cursor+page)
	case "g", "home":
		a.cursor = 0
	case "G", "end":
		a.cursor = len(a.visible) - 1
	case "tab", "shift+tab":
		favs := a.cfg.Favorites
		idx := 0
		for i, f := range favs {
			if r, ok := kube.Lookup(f); ok && r.Name == a.res.Name {
				idx = i
			}
		}
		if k.String() == "tab" {
			idx = (idx + 1) % len(favs)
		} else {
			idx = (idx - 1 + len(favs)) % len(favs)
		}
		if r, ok := kube.Lookup(favs[idx]); ok {
			return a.switchResource(r)
		}
	case "0":
		return a.openNavigator()
	case "1", "2", "3", "4", "5", "6", "7", "8", "9":
		i := int(k.String()[0] - '1')
		if i < len(a.cfg.Favorites) {
			if r, ok := kube.Lookup(a.cfg.Favorites[i]); ok {
				return a.switchResource(r)
			}
		}
	case ":":
		a.openInput(ovCommand, ":", func(s string) tea.Cmd { return a.runCommand(s) })
	case "/":
		a.openInput(ovFilter, "/", nil)
		a.input.SetValue(a.filter)
	case "?":
		a.ov = ovHelp
	case "r", "ctrl+r":
		return a.refresh()
	case "c":
		a.openPicker("Context", a.client.Contexts(), func(s string) tea.Cmd {
			return a.connect(s, "", "tab")
		})
	case "n":
		return fetchNamespaces(a.client)
	case "H":
		return a.openPulse()
	case "C":
		return a.openClusters()
	case "b":
		return a.openViewPicker()
	case "V":
		return a.openPolicy()
	case "A":
		a.openAI()
	case "T":
		names := ThemeNames()
		for i, n := range names {
			if n == a.st.T.Name {
				a.applyTheme(names[(i+1)%len(names)])
				break
			}
		}
	}
	if a.cursor < 0 {
		a.cursor = 0
	}
	row, ok := a.selected()
	if !ok {
		return nil
	}
	if _, _, hh := isHelmHistory(a.res); hh || a.res.Name == helmRes {
		return nil
	}
	if _, _, ok := isRollout(a.res); ok || a.res.Name == clustersRes || a.res.Name == policyRes {
		return nil // resource actions don't apply to these lists
	}
	return a.rowAction(k.String(), row)
}

func (a *App) rowAction(key string, row kube.Row) tea.Cmd {
	r, ns, name := a.res, row.Namespace, row.Name
	switch key {
	case "enter":
		if r.Name == "namespaces" {
			return a.setNamespace(name)
		}
		if r.Name == "customresourcedefinitions" { // open the objects of this CRD
			for _, d := range a.discovered[a.client.Context] {
				if d.Name == name {
					return a.switchResource(d)
				}
			}
			return discover(a.client, "crd:"+name)
		}
		if r.Name == "secrets" {
			return a.openSecret(ns, name)
		}
		return a.openText(r, ns, name, "describe")
	case "d":
		return a.openText(r, ns, name, "describe")
	case "y":
		return a.openText(r, ns, name, "yaml")
	case "h":
		switch {
		case r.Name == "nodes":
			return a.openNodeHealth(name)
		case r.Name == "pods":
			return a.openHealth(ns, name)
		case r.HasSelector():
			return fetchPods(a.client, r, ns, name, "health")
		}
	case "x":
		if r.Name == "secrets" {
			return a.openSecret(ns, name)
		}
	case "a":
		return a.analyze(r, ns, name)
	case "l", "p":
		if r.CanLogs() {
			purpose := "logs"
			if key == "p" {
				purpose = "prevlogs"
			}
			return fetchPods(a.client, r, ns, name, purpose)
		}
	case "L":
		if r.CanLogs() {
			split := a.split(false, nil)
			return tea.Batch(split, fetchPods(a.client, r, ns, name, "logs"))
		}
	case "s":
		if r.CanLogs() {
			return fetchPods(a.client, r, ns, name, "shell")
		}
	case "e":
		return a.openEdit(r, ns, name)
	case "ctrl+d", "D":
		if a.guardWrite() {
			a.askConfirm(fmt.Sprintf("Delete %s %s?", strings.ToLower(r.Kind), objName(ns, name)), name, func() tea.Cmd {
				return runAction(fmt.Sprintf("deleted %s/%s", r.Name, name), func(ctx context.Context) error { return a.client.Delete(ctx, r, ns, name) })
			})
		}
	case "S":
		if r.CanScale() && a.guardWrite() {
			a.openInput(ovInput, fmt.Sprintf("scale %s to replicas: ", name), func(s string) tea.Cmd {
				n, err := strconv.Atoi(strings.TrimSpace(s))
				if err != nil || n < 0 {
					a.setStatus("invalid replica count", true)
					return nil
				}
				return runAction(fmt.Sprintf("scaled %s to %d", name, n), func(ctx context.Context) error { return a.client.Scale(ctx, r, ns, name, n) })
			})
		}
	case "F":
		if canForward(r) {
			if direct, _ := kube.ForwardsFor(a.client.Context, r.Name, ns, name); len(direct) > 0 {
				return a.forwardMenu(direct, func() tea.Cmd { return fetchPortTargets(a.client, r, ns, name) })
			}
			return fetchPortTargets(a.client, r, ns, name)
		}
		a.setStatus("port-forward works on pods, services and deployments/statefulsets/daemonsets", true)
	case "u":
		if r.Name == "nodes" {
			return a.toggleCordon(name, strings.Contains(row.Cells[1], "SchedulingDisabled"))
		}
	case "X":
		if r.Name == "nodes" {
			return a.openDrainPreview(name)
		}
	case "o":
		if r.Name == "deployments" {
			return a.openRollout(ns, name)
		}
		a.setStatus("rollout history is available for deployments", true)
	case "R":
		if r.CanRestart() && a.guardWrite() {
			a.askConfirm(fmt.Sprintf("Rollout restart %s %s?", strings.ToLower(r.Kind), objName(ns, name)), "", func() tea.Cmd {
				return runAction("restarted "+name, func(ctx context.Context) error { return a.client.Restart(ctx, r, ns, name) })
			})
		}
	}
	return nil
}

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
	case "helm", "releases", "charts":
		return a.openHelm()
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

func (a *App) updateText(k tea.KeyMsg) tea.Cmd {
	if cmd, handled := a.updateEdit(k.String()); handled {
		return cmd
	}
	switch {
	case a.textKind == "drain" && k.String() == "A":
		return a.startDrain()
	case a.textKind == "nodehealth" && k.String() == "u":
		if n := a.target.name; n != "" {
			return a.toggleCordon(n, strings.Contains(strings.Join(a.textLines, " "), "cordoned"))
		}
	case a.textKind == "nodehealth" && k.String() == "X":
		return a.openDrainPreview(a.target.name)
	}
	switch k.String() {
	case "/":
		a.openInput(ovInput, "search › ", func(q string) tea.Cmd { a.setSearch(q); return nil })
		a.input.SetValue(a.search)
		a.input.CursorEnd()
		return nil
	case "n":
		a.nextMatch(1)
		return nil
	case "N":
		a.nextMatch(-1)
		return nil
	case "t":
		if a.textKind == "logs" && len(a.logSpec.pods) > 0 {
			return a.pickLogWindow()
		}
	case "T":
		if a.textKind == "logs" && len(a.logSpec.pods) > 0 {
			spec := a.logSpec
			spec.timestamps = !spec.timestamps
			return a.startLogs(spec)
		}
	case "esc", "q":
		if k.String() == "esc" && a.search != "" {
			a.search, a.matches = "", nil
			a.renderText(false)
			a.setStatus("search cleared", false)
			return nil
		}
		a.search, a.matches = "", nil
		a.stopLogs()
		a.reveal, a.secret = false, nil // never leave decoded secret values behind
		a.mode = a.prevMode
		if a.mode == modeText || a.mode == modeAI {
			a.mode = modeTable
		}
		return a.refresh()
	case "f":
		a.follow = !a.follow
		a.renderText(a.follow)
		return nil
	case "w":
		a.nowrap = !a.nowrap
		a.renderText(a.follow && a.textKind == "logs")
		return nil
	case "x":
		if a.textKind == "secret" || (a.textKind == "yaml" && a.target.res.Name == "secrets") {
			return a.toggleReveal()
		}
		if a.textKind == "helmvalues" && a.helmRel != nil {
			if a.reveal {
				a.reveal = false
				a.renderHelmText()
				return nil
			}
			a.askConfirm(fmt.Sprintf("Show unmasked Helm values of %s/%s on %s?", a.helmRel.Namespace, a.helmRel.Name, a.client.Context), a.helmRel.Name, func() tea.Cmd {
				a.reveal, a.revealedAt = true, time.Now()
				a.renderHelmText()
				return nil
			})
			return nil
		}
	case "h":
		if a.target.res.Name == "pods" && a.textKind != "health" {
			return a.openHealth(a.target.ns, a.target.name)
		}
	case "s":
		if a.target.res.Name == "pods" {
			return fetchPods(a.client, a.target.res, a.target.ns, a.target.name, "shell")
		}
	case "l":
		if a.textKind == "health" {
			return fetchPods(a.client, a.target.res, a.target.ns, a.target.name, "logs")
		}
	case "y":
		return tagPane(a.pane, fetchText(a.client, a.target.res, a.target.ns, a.target.name, "yaml", a.reveal))
	case "d":
		return tagPane(a.pane, fetchText(a.client, a.target.res, a.target.ns, a.target.name, "describe", false))
	case "a":
		a.stopLogs()
		return a.analyze(a.target.res, a.target.ns, a.target.name)
	case "g", "home":
		a.vp.GotoTop()
		return nil
	case "G", "end":
		a.vp.GotoBottom()
		return nil
	}
	var cmd tea.Cmd
	a.vp, cmd = a.vp.Update(k)
	if a.textKind == "logs" {
		a.follow = a.vp.AtBottom()
	}
	return cmd
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

func (a *App) updatePulse(k tea.KeyMsg) tea.Cmd {
	switch k.String() {
	case "esc", "q":
		a.mode = modeTable
		return a.refresh()
	case "up", "k":
		a.issueCursor = max(0, a.issueCursor-1)
	case "down", "j":
		a.issueCursor = min(len(a.issues)-1, a.issueCursor+1)
	case "r":
		a.pulseBusy = true
		return tagPane(a.pane, fetchPulse(a.client))
	case "f":
		return a.openForwards()
	case "t", "A":
		target, issues := a.client, a.issues // capture before focus moves to the AI pane
		a.openAI()
		a.aiClient = target
		a.ag = agentState{}
		a.aiBusy = true
		a.chat = append(a.chat, chatEntry{role: "user", display: "Triage all detected issues on " + target.Context + "…"})
		return triageForAI(target, issues)
	case "enter", "a":
		if a.issueCursor < len(a.issues) {
			is := a.issues[a.issueCursor]
			if r, ok := kube.Lookup(is.Kind); ok {
				return a.analyze(r, is.Namespace, is.Name)
			}
		}
	case "l":
		if a.issueCursor < len(a.issues) {
			is := a.issues[a.issueCursor]
			if r, ok := kube.Lookup(is.Kind); ok && r.CanLogs() {
				return fetchPods(a.client, r, is.Namespace, is.Name, "logs")
			}
		}
	case "d":
		if a.issueCursor < len(a.issues) {
			is := a.issues[a.issueCursor]
			if r, ok := kube.Lookup(is.Kind); ok {
				return a.openText(r, is.Namespace, is.Name, "describe")
			}
		}
	case ":":
		a.openInput(ovCommand, ":", func(s string) tea.Cmd { return a.runCommand(s) })
	case "?":
		a.ov = ovHelp
	}
	if a.issueCursor < 0 {
		a.issueCursor = 0
	}
	return nil
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

func (a *App) updateAI(k tea.KeyMsg) tea.Cmd {
	switch k.String() {
	case "esc":
		if a.aiBusy {
			a.stopAI()
			a.chat = append(a.chat, chatEntry{role: "error", display: "stopped"})
			a.renderChat()
			return nil
		}
		a.aiInput.Blur()
		a.mode = a.prevMode
		if a.mode == modeAI {
			a.mode = modeTable
		}
		return nil
	case "enter":
		q := strings.TrimSpace(a.aiInput.Value())
		if q == "" || a.aiBusy {
			return nil
		}
		a.aiInput.SetValue("")
		if strings.HasPrefix(q, ":") {
			return a.runCommand(strings.TrimPrefix(q, ":"))
		}
		a.ag = agentState{} // a new question gets a fresh step budget
		return a.ask(q)
	case "ctrl+r":
		if a.aiBusy {
			return nil
		}
		return a.pickSuggested()
	case "ctrl+n", "ctrl+l":
		a.stopAI()
		a.chat = nil
		a.renderChat()
		return nil
	case "ctrl+o":
		a.pickProfile()
		return nil
	case "pgup", "pgdown", "up", "down", "ctrl+u", "ctrl+d":
		var cmd tea.Cmd
		a.aiVP, cmd = a.aiVP.Update(k)
		return cmd
	}
	var cmd tea.Cmd
	a.aiInput, cmd = a.aiInput.Update(k)
	return cmd
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

func (a *App) updateOverlay(k tea.KeyMsg) tea.Cmd {
	switch a.ov {
	case ovApprove:
		return a.updateApprove(k)
	case ovHelp:
		a.ov = ovNone
		return nil

	case ovConfirm:
		if a.inputHint != "" { // type-to-confirm
			switch k.String() {
			case "esc":
				a.ov = ovNone
				a.setStatus("cancelled", false)
				return nil
			case "enter":
				a.ov = ovNone
				if a.input.Value() == a.inputHint {
					return a.onConfirm()
				}
				a.setStatus("name did not match — cancelled", true)
				return nil
			}
			var cmd tea.Cmd
			a.input, cmd = a.input.Update(k)
			return cmd
		}
		a.ov = ovNone
		if k.String() == "y" || k.String() == "Y" {
			return a.onConfirm()
		}
		a.setStatus("cancelled", false)
		return nil

	case ovPicker:
		items := a.picker.visibleItems()
		switch k.String() {
		case "esc":
			a.ov = ovNone
		case "up", "ctrl+k":
			a.picker.cursor = max(0, a.picker.cursor-1)
		case "down", "ctrl+j":
			a.picker.cursor = min(len(items)-1, a.picker.cursor+1)
		case "enter":
			a.ov = ovNone
			if a.picker.cursor < len(items) && a.picker.cursor >= 0 {
				return a.picker.onSelect(items[a.picker.cursor])
			}
		case "backspace":
			if n := len(a.picker.filter); n > 0 {
				a.picker.filter = a.picker.filter[:n-1]
				a.picker.cursor = 0
			}
		default:
			if k.Type == tea.KeyRunes {
				a.picker.filter += string(k.Runes)
				a.picker.cursor = 0
			}
		}
		return nil

	default: // command, filter, generic input
		switch k.String() {
		case "esc":
			if a.ov == ovFilter {
				a.filter = ""
				a.applyFilter()
			}
			a.ov = ovNone
			a.clearSecretInput()
			return nil
		case "enter":
			kind := a.ov
			a.ov = ovNone
			if kind == ovFilter {
				return nil
			}
			value := a.input.Value()
			a.clearSecretInput() // never keep a typed key around in the input widget
			if a.onInput != nil {
				return a.onInput(value)
			}
			return nil
		}
		var cmd tea.Cmd
		a.input, cmd = a.input.Update(k)
		if a.ov == ovFilter {
			a.filter = a.input.Value()
			a.cursor = 0
			a.applyFilter()
		}
		return cmd
	}
}
