package ui

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"kubesage/internal/agent"
	"kubesage/internal/ai"
	"kubesage/internal/kube"
)

// ---------- messages ----------

type cmdResultMsg struct {
	pane   *pane
	gen    int
	result agent.Result
	manual bool // run via "!" from the table, show output in the text view
}
type pfTargetsMsg struct {
	targets []kube.PortTarget
	err     error
}
type pfStartedMsg struct {
	fw  *kube.Forward
	err error
}

// agentState tracks one batch of commands proposed by the AI (or picked by the user).
type agentState struct {
	queue       []agent.Command
	results     []agent.Result
	idx         int
	steps       int
	approveAll  bool
	typing      bool // danger context: waiting for "approve" to be typed
	running     bool
	userStarted bool
}

// aiTarget is the cluster AI commands run against: the one the conversation is about.
func (a *App) aiTarget() *kube.Client {
	if a.aiClient != nil {
		return a.aiClient
	}
	return a.client
}

func (a *App) runner() agent.Runner {
	return agent.Runner{BaseArgs: a.aiTarget().KubectlArgs(), Timeout: 60 * time.Second}
}

// manualRunner runs "!" commands on the focused pane's cluster.
func (a *App) manualRunner() agent.Runner {
	return agent.Runner{BaseArgs: a.client.KubectlArgs(), Timeout: 60 * time.Second}
}

func (a *App) systemPrompt() string {
	if a.cfg.AI.AllowCommands {
		return ai.SystemPrompt + agent.ToolInstructions
	}
	return ai.SystemPrompt
}

func (a *App) execCommand(c agent.Command) (string, error) {
	if a.execFn != nil {
		return a.execFn(c)
	}
	return a.runner().Run(context.Background(), c)
}

func (a *App) execManual(c agent.Command) (string, error) {
	if a.execFn != nil {
		return a.execFn(c)
	}
	return a.manualRunner().Run(context.Background(), c)
}

func runAgentCommand(exec func(agent.Command) (string, error), c agent.Command, gen int, manual bool) tea.Cmd {
	return func() tea.Msg {
		out, err := exec(c)
		return cmdResultMsg{gen: gen, result: agent.Result{Command: c, Output: out, Err: err}, manual: manual}
	}
}

// afterReply is called when an assistant reply finishes streaming.
func (a *App) afterReply(reply string) tea.Cmd {
	if !a.cfg.AI.AllowCommands {
		return nil
	}
	cmds := agent.ExtractRun(reply)
	if len(cmds) == 0 {
		return nil
	}
	if a.ag.steps >= a.cfg.AI.MaxSteps {
		a.chat = append(a.chat, chatEntry{role: "error", display: fmt.Sprintf(
			"step limit reached (%d command rounds). Ask a follow-up to let the AI continue.", a.cfg.AI.MaxSteps)})
		a.renderChat()
		return nil
	}
	a.ag = agentState{queue: cmds, steps: a.ag.steps + 1}
	return a.processQueue()
}

// startUserCommands runs commands the user picked from a reply.
func (a *App) startUserCommands(cmds []agent.Command) tea.Cmd {
	a.ag = agentState{queue: cmds, steps: a.ag.steps, userStarted: true}
	return a.processQueue()
}

// processQueue advances through the batch, pausing for approvals.
func (a *App) processQueue() tea.Cmd {
	for a.ag.idx < len(a.ag.queue) {
		c := a.ag.queue[a.ag.idx]
		switch {
		case c.Risk == agent.Blocked:
			a.recordResult(agent.Result{Command: c})
			continue
		case c.Risk == agent.Write && a.readOnly():
			a.recordResult(agent.Result{Command: c, Declined: true, Note: "kubesage is in read-only mode; changes are disabled"})
			continue
		case c.Risk == agent.Read && (a.cfg.AI.AutoRunReadOnly || a.ag.userStarted), a.ag.approveAll:
			return a.execQueued(c)
		default:
			a.ov = ovApprove
			a.ag.typing = false
			a.input.SetValue("")
			return nil
		}
	}
	// Batch finished: hand results back to the model.
	results := a.ag.results
	a.ag.queue, a.ag.results, a.ag.idx = nil, nil, 0
	if len(results) == 0 {
		return nil
	}
	return a.sendToAI(fmt.Sprintf("↳ returned %d command result(s) to the AI", len(results)), agent.FormatResults(results))
}

func (a *App) execQueued(c agent.Command) tea.Cmd {
	a.ag.running = true
	a.aiBusy = true
	a.renderChat()
	return runAgentCommand(a.execCommand, c, a.aiGen, false)
}

func (a *App) recordResult(r agent.Result) {
	a.ag.results = append(a.ag.results, r)
	a.ag.idx++
	a.chat = append(a.chat, chatEntry{role: "tool", display: toolDisplay(r)})
	a.renderChat()
}

func toolDisplay(r agent.Result) string {
	head := "$ " + r.Command.Display() + "   [" + r.Command.Risk.String() + "]"
	switch {
	case r.Declined:
		note := "declined"
		if r.Note != "" {
			note = r.Note
		}
		return head + "\n✗ not run: " + note
	case r.Command.Risk == agent.Blocked:
		return "$ " + r.Command.Raw + "   [blocked]\n✗ " + r.Command.Reason
	}
	lines := strings.Split(strings.TrimRight(r.Output, "\n"), "\n")
	if len(lines) > 14 {
		lines = append(lines[:14], fmt.Sprintf("… %d more lines sent to the AI", len(lines)-14))
	}
	body := strings.Join(lines, "\n")
	if r.Err != nil {
		body += "\n✗ " + r.Err.Error()
	}
	return head + "\n" + body
}

func (a *App) handleCmdResult(msg cmdResultMsg) tea.Cmd {
	if msg.manual {
		a.textKind = "output"
		a.textTitle = msg.result.Command.Display()
		out := msg.result.Output
		if msg.result.Err != nil {
			out += "\n[kubesage] " + msg.result.Err.Error()
		}
		a.textLines = strings.Split(strings.TrimRight(out, "\n"), "\n")
		a.renderText(true)
		return nil
	}
	if msg.gen != a.aiGen {
		return nil
	}
	a.ag.running = false
	a.aiBusy = false
	a.recordResult(msg.result)
	return a.processQueue()
}

// ---------- approval overlay ----------

func (a *App) currentProposal() (agent.Command, bool) {
	if a.ag.idx < len(a.ag.queue) {
		return a.ag.queue[a.ag.idx], true
	}
	return agent.Command{}, false
}

func (a *App) updateApprove(k tea.KeyMsg) tea.Cmd {
	c, ok := a.currentProposal()
	if !ok {
		a.ov = ovNone
		return nil
	}
	needTyping := a.dangerFor(a.aiTarget().Context) && c.Risk == agent.Write
	if a.ag.typing {
		switch k.String() {
		case "esc":
			a.ag.typing = false
			a.input.SetValue("")
			return nil
		case "enter":
			if strings.TrimSpace(a.input.Value()) != "approve" {
				a.setStatus(`type "approve" to run it on this production-like context`, true)
				return nil
			}
			a.ov = ovNone
			return a.execQueued(c)
		}
		var cmd tea.Cmd
		a.input, cmd = a.input.Update(k)
		return cmd
	}
	switch k.String() {
	case "y", "Y", "enter":
		if needTyping {
			a.ag.typing = true
			a.input.SetValue("")
			a.input.Focus()
			return nil
		}
		a.ov = ovNone
		return a.execQueued(c)
	case "a", "A":
		if needTyping {
			a.setStatus("approve-all is disabled on production-like contexts", true)
			return nil
		}
		a.ag.approveAll = true
		a.ov = ovNone
		return a.execQueued(c)
	case "n", "N":
		a.ov = ovNone
		a.recordResult(agent.Result{Command: c, Declined: true})
		return a.processQueue()
	case "e", "E":
		a.ov = ovNone
		a.openInput(ovInput, "edit › ", func(s string) tea.Cmd {
			edited := agent.Parse(s)
			if edited.Risk == agent.Blocked {
				a.recordResult(agent.Result{Command: edited})
				return a.processQueue()
			}
			if edited.Risk == agent.Write && a.readOnly() {
				a.recordResult(agent.Result{Command: edited, Declined: true, Note: "kubesage is in read-only mode"})
				return a.processQueue()
			}
			a.ag.queue[a.ag.idx] = edited
			return a.execQueued(edited) // editing it yourself counts as approval
		})
		a.input.SetValue(c.Display())
		a.input.CursorEnd()
		return nil
	case "esc":
		// Decline this and everything after it, then let the AI know.
		a.ov = ovNone
		for a.ag.idx < len(a.ag.queue) {
			a.recordResult(agent.Result{Command: a.ag.queue[a.ag.idx], Declined: true, Note: "the user stopped this batch"})
		}
		return a.processQueue()
	}
	return nil
}

// pickSuggested lets the user run a command from the last AI reply.
func (a *App) pickSuggested() tea.Cmd {
	var reply string
	for i := len(a.chat) - 1; i >= 0; i-- {
		if a.chat[i].role == "assistant" && a.chat[i].content != "" {
			reply = a.chat[i].content
			break
		}
	}
	cmds := agent.ExtractSuggested(reply)
	if len(cmds) == 0 {
		a.setStatus("no kubectl commands in the last AI reply", true)
		return nil
	}
	labels := make([]string, len(cmds))
	for i, c := range cmds {
		tag := "[read] "
		switch c.Risk {
		case agent.Write:
			tag = "[change] "
		case agent.Blocked:
			tag = "[blocked] "
		}
		labels[i] = tag + c.Display()
		if c.Risk == agent.Blocked {
			labels[i] = tag + c.Raw
		}
	}
	a.openPicker("Run a command from the AI reply", labels, func(s string) tea.Cmd {
		for i, l := range labels {
			if l == s {
				return a.startUserCommands([]agent.Command{cmds[i]})
			}
		}
		return nil
	})
	return nil
}

// runManual executes a kubectl command typed by the user with "!".
func (a *App) runManual(line string) tea.Cmd {
	line = strings.TrimSpace(line)
	if !strings.HasPrefix(line, "kubectl ") {
		line = "kubectl " + line
	}
	c := agent.Parse(line)
	switch {
	case c.Risk == agent.Blocked:
		a.setStatus("blocked: "+c.Reason, true)
		return nil
	case c.Risk == agent.Write && !a.guardWrite():
		return nil
	}
	run := func() tea.Cmd {
		a.stopLogs()
		if a.mode != modeText {
			a.prevMode = a.mode
		}
		a.mode = modeText
		a.textKind, a.textTitle, a.textLines = "output", "running "+c.Display()+"…", nil
		a.renderText(true)
		return tagPane(a.pane, runAgentCommand(a.execManual, c, a.aiGen, true))
	}
	if c.Risk == agent.Write {
		a.askConfirm("Run on "+a.client.Context+"?\n  "+c.Display(), "approve", run)
		return nil
	}
	return run()
}

// ---------- port-forward ----------

func fetchPortTargets(c *kube.Client, r kube.Resource, ns, name string) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		t, err := c.PortTargets(ctx, r, ns, name)
		return pfTargetsMsg{targets: t, err: err}
	}
}

func startForward(c *kube.Client, t kube.PortTarget, local, remote int) tea.Cmd {
	return func() tea.Msg {
		f, err := c.PortForward(t, local, remote)
		return pfStartedMsg{fw: f, err: err}
	}
}

func canForward(r kube.Resource) bool {
	return r.Name == "pods" || r.Name == "services" || r.HasSelector() && r.Name != "jobs"
}

// defaultLocal proposes a free local port: the exposed port itself, or 8000+port
// for privileged ports, moving up if something on this machine already uses it.
func defaultLocal(exposed int) int {
	p := exposed
	if p > 0 && p < 1024 {
		p += 8000
	}
	if free := kube.SuggestLocalPort(p); free > 0 {
		return free
	}
	return p
}

func (a *App) handlePortTargets(msg pfTargetsMsg) tea.Cmd {
	if msg.err != nil {
		a.setStatus("port-forward: "+msg.err.Error(), true)
		return nil
	}
	ask := func(t kube.PortTarget) tea.Cmd {
		hint := fmt.Sprintf("forward %s  local:remote › ", t.ObjectDesc)
		a.openInput(ovInput, hint, func(s string) tea.Cmd {
			local, remote, err := parsePorts(s, t.Port)
			if err != nil {
				a.setStatus(err.Error(), true)
				return nil
			}
			a.setStatus(fmt.Sprintf("starting port-forward to %s…", t.ObjectDesc), false)
			return startForward(a.client, t, local, remote)
		})
		if t.Port > 0 {
			a.input.SetValue(fmt.Sprintf("%d:%d", defaultLocal(t.ExposedAs), t.Port))
			a.input.CursorEnd()
		}
		return nil
	}
	if len(msg.targets) == 1 {
		return ask(msg.targets[0])
	}
	labels := make([]string, len(msg.targets))
	for i, t := range msg.targets {
		labels[i] = t.Label()
	}
	a.openPicker("Port to forward", labels, func(s string) tea.Cmd {
		for i, l := range labels {
			if l == s {
				return ask(msg.targets[i])
			}
		}
		return nil
	})
	return nil
}

// parsePorts accepts "8080:80", "8080" (same remote) or ":80" (random local port).
func parsePorts(s string, defRemote int) (int, int, error) {
	s = strings.TrimSpace(s)
	l, r, hasColon := strings.Cut(s, ":")
	if !hasColon {
		r = l
	}
	var local, remote int
	var err error
	if l != "" {
		if local, err = strconv.Atoi(l); err != nil || local < 0 || local > 65535 {
			return 0, 0, fmt.Errorf("invalid local port %q", l)
		}
	}
	if r == "" {
		remote = defRemote
	} else if remote, err = strconv.Atoi(r); err != nil {
		return 0, 0, fmt.Errorf("invalid remote port %q", r)
	}
	if remote <= 0 || remote > 65535 {
		return 0, 0, fmt.Errorf("remote port required, e.g. 8080:80")
	}
	return local, remote, nil
}

func runningForwards() []*kube.Forward {
	var out []*kube.Forward
	for _, f := range kube.Forwards() {
		if f.Running() {
			out = append(out, f)
		}
	}
	return out
}

// forwardVisible reports whether f's object is in the context/namespace being viewed.
func forwardVisible(f *kube.Forward, c *kube.Client) bool {
	return f.Context == c.Context && (c.Namespace == kube.AllNamespaces || c.Namespace == f.Namespace)
}

// forwardLabel describes a forward relative to what the user is looking at.
func forwardLabel(f *kube.Forward, c *kube.Client) string {
	s := fmt.Sprintf("%s → %s", f.ObjectDesc, f.URL())
	where := []string{}
	if f.Context != c.Context {
		where = append(where, "ctx "+f.Context)
	}
	if f.Namespace != c.Namespace || c.Namespace == kube.AllNamespaces {
		where = append(where, "ns "+f.Namespace)
	}
	if len(where) > 0 {
		s += "  (" + strings.Join(where, ", ") + ")"
	}
	return s
}

// forwardPill is the header summary: names the target when there is one forward.
func (a *App) forwardPill() string {
	fws := runningForwards()
	switch len(fws) {
	case 0:
		return ""
	case 1:
		f := fws[0]
		s := fmt.Sprintf("⇄ %s :%d", f.ObjectDesc, f.Local)
		if !forwardVisible(f, a.client) {
			if f.Context != a.client.Context {
				s += " @" + f.Context
			} else {
				s += " in " + f.Namespace
			}
		}
		return s + "  f"
	}
	here := 0
	for _, f := range fws {
		if forwardVisible(f, a.client) {
			here++
		}
	}
	s := fmt.Sprintf("⇄ %d forwards", len(fws))
	if here < len(fws) {
		s += fmt.Sprintf(" · %d elsewhere", len(fws)-here)
	}
	return s + "  f"
}

// openForwards lists every forward (key f or :pf): what it targets, where, its URL and uptime.
func (a *App) openForwards() tea.Cmd {
	fws := kube.Forwards()
	if len(fws) == 0 {
		a.setStatus("no port-forwards; press F on a pod, service or deployment to start one", false)
		return nil
	}
	labels := make([]string, len(fws))
	for i, f := range fws {
		state := "up " + kube.Age(metav1Time(f.Started))
		if !f.Running() {
			state = "stopped"
			if f.Err() != nil {
				state += ": " + f.Err().Error()
			}
		}
		labels[i] = fmt.Sprintf("#%d  %s  · %s", f.ID, forwardLabel(f, a.client), state)
	}
	labels = append(labels, "Stop all port-forwards")
	a.openPicker("Port-forwards  (enter: open · go to · stop)", labels, func(s string) tea.Cmd {
		if s == labels[len(labels)-1] {
			a.askConfirm(fmt.Sprintf("Stop all %d port-forwards?", len(fws)), "", func() tea.Cmd {
				for _, f := range fws {
					kube.RemoveForward(f.ID)
				}
				a.setStatus("stopped all port-forwards", false)
				return nil
			})
			return nil
		}
		for i, l := range labels {
			if l == s {
				return a.forwardMenu([]*kube.Forward{fws[i]}, nil)
			}
		}
		return nil
	})
	return nil
}

// gotoForward jumps to the forwarded object's row, switching context/namespace if needed.
func (a *App) gotoForward(f *kube.Forward) tea.Cmd {
	r, ok := kube.Lookup(f.Resource)
	if !ok {
		r, _ = kube.Lookup("pods")
	}
	a.pending = &targetRef{res: r, ns: f.Namespace, name: f.Object}
	a.mode = modeTable
	if f.Context != a.client.Context {
		a.res = r
		a.setStatus("switching to context "+f.Context+"…", false)
		return a.connect(f.Context, f.Namespace, "tab")
	}
	if a.client.Namespace != kube.AllNamespaces && a.client.Namespace != f.Namespace {
		a.client.Namespace = f.Namespace
	}
	return a.switchResource(r)
}

// applyPendingSelect moves the cursor to the row requested by gotoForward.
func (a *App) applyPendingSelect() {
	p := a.pending
	if p == nil || p.res.Name != a.res.Name {
		return
	}
	a.pending = nil
	for i, idx := range a.visible {
		if r := a.table.Rows[idx]; r.Name == p.name && (r.Namespace == p.ns || !p.res.Namespaced) {
			a.cursor = i
			a.setStatus("⇄ "+p.res.Kind+" "+p.name+" is the forwarded object", false)
			return
		}
	}
	a.setStatus(p.res.Kind+" "+p.name+" no longer exists (the forward points at pod "+p.name+")", true)
}

// forwardMenu offers open/stop for existing forwards and, optionally, starting another one.
func (a *App) forwardMenu(fws []*kube.Forward, startNew func() tea.Cmd) tea.Cmd {
	type action struct {
		label string
		run   func() tea.Cmd
	}
	var acts, stops []action
	for _, f := range fws {
		f := f
		if f.Running() {
			acts = append(acts, action{"Open " + f.URL() + " in browser  (" + f.ObjectDesc + ")", func() tea.Cmd {
				if err := openBrowserFn(f.URL()); err != nil {
					a.setStatus("could not open browser: "+err.Error()+" — visit "+f.URL(), true)
				} else {
					a.setStatus("opened "+f.URL(), false)
				}
				return nil
			}})
		}
		stops = append(stops, action{fmt.Sprintf("Stop #%d  %s", f.ID, f.String()), func() tea.Cmd {
			kube.RemoveForward(f.ID)
			a.setStatus("stopped "+f.String(), false)
			return nil
		}})
	}
	if startNew != nil {
		acts = append(acts, action{"Start another port-forward…", startNew})
	} else {
		// Opened from the forwards list: offer to jump to the object.
		for _, f := range fws {
			f := f
			acts = append(acts, action{"Go to " + f.ObjectDesc + " in the table", func() tea.Cmd { return a.gotoForward(f) }})
		}
	}
	acts = append(acts, stops...)
	labels := make([]string, len(acts))
	for i, ac := range acts {
		labels[i] = ac.label
	}
	title := "Port-forward"
	if len(fws) == 1 {
		title = "Already forwarding " + fws[0].ObjectDesc
	}
	a.openPicker(title, labels, func(s string) tea.Cmd {
		for i, l := range labels {
			if l == s {
				return acts[i].run()
			}
		}
		return nil
	})
	return nil
}

// checkForwards reports forwards that died since the last tick.
func (a *App) checkForwards() {
	for _, f := range kube.Forwards() {
		if !f.Running() && !a.pfReported[f.ID] {
			a.pfReported[f.ID] = true
			if err := f.Err(); err != nil {
				a.setStatus(fmt.Sprintf("port-forward #%d stopped: %v (:pf to manage)", f.ID, err), true)
			}
		}
	}
}

func metav1Time(t time.Time) metav1.Time { return metav1.NewTime(t) }

func activeForwards() int {
	n := 0
	for _, f := range kube.Forwards() {
		if f.Running() {
			n++
		}
	}
	return n
}
