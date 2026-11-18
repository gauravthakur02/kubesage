// kubesage is a terminal Kubernetes IDE with an AI troubleshooting copilot.
package main

import (
	"bufio"
	"context"
	"encoding/base64"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"runtime"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"
	"golang.org/x/term"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/gauravthakur02/kubesage/internal/agent"
	"github.com/gauravthakur02/kubesage/internal/ai"
	"github.com/gauravthakur02/kubesage/internal/audit"
	"github.com/gauravthakur02/kubesage/internal/bot"
	"github.com/gauravthakur02/kubesage/internal/cli"
	"github.com/gauravthakur02/kubesage/internal/config"
	"github.com/gauravthakur02/kubesage/internal/diff"
	"github.com/gauravthakur02/kubesage/internal/kube"
	"github.com/gauravthakur02/kubesage/internal/mcp"
	"github.com/gauravthakur02/kubesage/internal/report"
	"github.com/gauravthakur02/kubesage/internal/serve"
	"github.com/gauravthakur02/kubesage/internal/ui"
	"github.com/gauravthakur02/kubesage/internal/update"
	"github.com/gauravthakur02/kubesage/internal/web"
)

var version = "0.1.0-dev"

// buildVersion is the version set by GoReleaser or Homebrew (-X main.version), else the module version that
// `go install github.com/gauravthakur02/kubesage@v0.2.0` records, else the dev placeholder.
func buildVersion() string {
	if version != "0.1.0-dev" {
		return version
	}
	if bi, ok := debug.ReadBuildInfo(); ok && strings.HasPrefix(bi.Main.Version, "v") {
		return strings.TrimPrefix(bi.Main.Version, "v")
	}
	return version
}

const usage = `kubesage — a Kubernetes IDE for your terminal, with an AI copilot

Usage:
  kubesage [flags]                 launch the interactive UI
  kubesage scan [--policy] [flags] print detected issues (exit code 2 if errors found);
                                   --policy adds security/config checks of every workload
  kubesage ask [flags] "question"  ask the AI about the cluster, answer streams to stdout
  kubesage ai                      list AI profiles and whether their API key is found
  kubesage ai test [profile]       send a tiny prompt to check a profile works
  kubesage ai add <name> --provider anthropic|openai|azure|gemini|ollama|openai-compatible
                  [--model m] [--base-url u] [--api-version v] [--key-env VAR] [--no-key]
                                   add a profile and (optionally) store its key
  kubesage ai set-key <profile>    store an API key in the OS keychain (prompts, or reads stdin)
  kubesage ai delete-key <profile> remove a stored key
  kubesage ai use <profile>        make a profile the default
  kubesage ai remove <profile>     delete a profile
  kubesage config init             write a starter config file
  kubesage config path             print the config file location
  kubesage mcp [--context c] [-n ns]     serve read-only tools to AI agents over MCP (stdio)
  kubesage report [--post]         cluster summary (issues, expiring certs); --post sends it to the
                                   Slack/Teams webhook in $<reports.webhook_env> (run it from cron)
  kubesage schedule run [--dry-run]   apply scale-to-zero schedules now (run from cron every 10 min)
  kubesage schedule status         show what each schedule wants right now
  kubesage approve list | <id> [--reject]   two-person rule: list or decide pending changes
  kubesage audit verify            check the audit log's hash chain (exit 1 if tampered)
  kubesage audit export [--since 24h]       push the audit log to Grafana Loki (audit_loki_url)
  kubesage serve [--addr 127.0.0.1:7777] [--lan] [--allow-changes]
                                   the full UI in a web browser (read-only unless --allow-changes)
  kubesage snapshot save [name] | list | diff <name>   save a namespace (-n) and compare it later
  kubesage bot [--addr :8090]      read-only Slack slash command (/slack) and Teams webhook (/teams)
  kubesage update [--check]        download, verify and install the latest release
  kubesage completion bash|zsh|fish|powershell   shell completion script
  kubesage man                     print the man page (kubesage man > kubesage.1)
  kubesage version

Flags:
`

func main() {
	version = buildVersion()
	fs := flag.NewFlagSet("kubesage", flag.ExitOnError)
	kubeconfig := fs.String("kubeconfig", "", "path to kubeconfig (default: $KUBECONFIG or ~/.kube/config)")
	kctx := fs.String("context", "", "kube context(s); comma-separate to open each cluster in its own tab (default: current context)")
	ns := fs.String("n", "", "namespace (default: from context); use \"all\" for all namespaces")
	allNS := fs.Bool("A", false, "all namespaces")
	res := fs.String("r", "", "resource to open first, e.g. pods, deploy, nodes")
	readOnly := fs.Bool("readonly", false, "disable every change to the cluster (UI actions and AI commands)")
	cfgPath := fs.String("config", "", "config file (default: "+config.DefaultPath()+")")
	profile := fs.String("ai", "", "AI profile to use (overrides config ai.active)")
	startCmd := fs.String("cmd", "", "kubesage command to run at startup, e.g. \":apply ./k8s\" or \":dev ./k8s\"")
	theme := fs.String("theme", "", "colour theme: "+strings.Join(ui.ThemeNames(), ", "))
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, ui.PrintableLogo("version "+version))
		fmt.Fprint(os.Stderr, "\n"+usage)
		fs.PrintDefaults()
	}

	args := os.Args[1:]
	sub := ""
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		sub, args = args[0], args[1:]
	}
	if sub == "config" {
		// Accept flags before or after the action: `config init --config x` and `config --config x init`.
		cfs := flag.NewFlagSet("config", flag.ExitOnError)
		path := cfs.String("config", "", "config file")
		_ = cfs.Parse(args)
		rest := cfs.Args()
		if len(rest) > 1 {
			_ = cfs.Parse(rest[1:])
			rest = append(rest[:1], cfs.Args()...)
		}
		runConfig(rest, *path)
		return
	}
	if sub != "" && sub != "__complete" && !cli.Known(sub) {
		fmt.Fprintf(os.Stderr, "kubesage %s does not have a %q command.\n", version, sub)
		if update.IsRelease(version) {
			fmt.Fprintln(os.Stderr, "It may be newer than this version: update with `kubesage update` (or brew upgrade kubesage).")
		}
		fmt.Fprintln(os.Stderr, "Run `kubesage help` for the commands this version has.")
		os.Exit(1)
	}
	switch sub {
	case "completion":
		shell := ""
		if len(args) > 0 {
			shell = args[0]
		}
		script, err := cli.Completion(shell)
		exitIf(err)
		fmt.Print(script)
		return
	case "man":
		fmt.Print(cli.Man(version, time.Now()))
		return
	case "update":
		runUpdate(args)
		return
	case "__complete": // used by the completion scripts
		runComplete(args)
		return
	}
	if sub == "version" {
		if term.IsTerminal(int(os.Stdout.Fd())) && os.Getenv("NO_COLOR") == "" {
			ui.AnimateLogo(os.Stdout, "version "+version, 1500*time.Millisecond)
		} else {
			fmt.Println(ui.PrintableLogo("version " + version))
		}
		if update.IsRelease(version) && os.Getenv("KUBESAGE_NO_UPDATE_CHECK") == "" {
			if r, err := update.Latest(context.Background()); err == nil && update.Newer(version, r.Version) {
				fmt.Printf("\nkubesage %s is available: %s\n", r.Version, r.URL)
			}
		}
		return
	}
	// Global flags may appear anywhere, e.g. `kubesage ai add x --provider anthropic --config c.yaml`.
	for _, a := range args {
		if a == "-h" || a == "--help" || a == "-help" || a == "help" {
			if sub == "" || sub == "help" {
				fs.Usage()
				return
			}
		}
	}
	if sub == "help" {
		fs.Usage()
		return
	}
	globals, rest := splitGlobalFlags(fs, args)
	_ = fs.Parse(globals)
	subArgs := rest

	cfg, err := config.Load(*cfgPath)
	exitIf(err)
	if cfg.Team.Repo != "" && sub != "completion" && sub != "__complete" {
		if _, err := cfg.SyncTeam(context.Background()); err != nil {
			fmt.Fprintln(os.Stderr, "⚠ "+err.Error())
		}
	}
	if *profile != "" {
		cfg.AI.Active = *profile
	}
	if *theme != "" {
		cfg.Theme = *theme
	}
	if sub == "ai" {
		runAI(cfg, subArgs)
		return
	}
	namespace := *ns
	if *allNS || namespace == "all" {
		namespace = "*all*"
	}
	contexts := strings.Split(*kctx, ",")
	client, err := kube.New(*kubeconfig, strings.TrimSpace(contexts[0]), strings.ReplaceAll(namespace, "*all*", ""))
	exitIf(err)
	if namespace == "*all*" {
		client.Namespace = kube.AllNamespaces
	}

	switch sub {
	case "":
		var extra []string
		for _, c := range contexts[1:] {
			if c = strings.TrimSpace(c); c != "" {
				extra = append(extra, c)
			}
		}
		// No --context: let the user pick cluster(s) in the UI (unless there is only one).
		choose := *kctx == "" && *startCmd == "" && cfg.ChooseContext && len(client.Contexts()) > 1
		app := ui.New(cfg, client, ui.Options{Kubeconfig: *kubeconfig, Context: contexts[0], Namespace: strings.ReplaceAll(namespace, "*all*", "all"),
			Resource: *res, ReadOnly: *readOnly, ExtraContexts: extra, ChooseContext: choose, Version: version,
			Tour: !cfg.TourDone && term.IsTerminal(int(os.Stdout.Fd())) && *startCmd == "", Command: strings.TrimPrefix(*startCmd, ":")})
		_, err := tea.NewProgram(app, tea.WithAltScreen()).Run()
		exitIf(err)
	case "mcp":
		// MCP over stdio: stdout carries the protocol, so nothing else may be printed there.
		srv := &mcp.Server{Version: version, Kubeconfig: *kubeconfig, Default: client}
		exitIf(srv.Serve(context.Background(), os.Stdin, os.Stdout))
	case "scan":
		code := runScan(client)
		for _, a := range subArgs {
			if a == "--policy" || a == "-policy" {
				if pc := runPolicyScan(cfg, client); pc > code {
					code = pc
				}
			}
		}
		os.Exit(code)
	case "serve":
		runServe(cfg, *cfgPath, *kubeconfig, *kctx, namespace, *readOnly, subArgs)
	case "snapshot":
		runSnapshot(cfg, *cfgPath, client, subArgs)
	case "bot":
		runBot(cfg, *kubeconfig, client, subArgs)
	case "approve":
		runApprove(cfg, client, subArgs)
	case "audit":
		runAudit(cfg, subArgs)
	case "report":
		runReport(cfg, *kubeconfig, client, subArgs)
	case "schedule":
		runSchedule(cfg, *kubeconfig, subArgs)
	case "ask":
		runAsk(cfg, client, strings.Join(subArgs, " "), *readOnly || cfg.ReadOnly)
	default:
		fs.Usage()
		os.Exit(1)
	}
}

func exitIf(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, lipgloss.NewStyle().Foreground(lipgloss.Color("#f7768e")).Render("✗ "+err.Error()))
		os.Exit(1)
	}
}

func runConfig(args []string, path string) {
	if path == "" {
		path = config.DefaultPath()
	}
	if len(args) == 0 || args[0] == "path" {
		fmt.Println(path)
		return
	}
	if args[0] != "init" {
		exitIf(fmt.Errorf("unknown config command %q (use init or path)", args[0]))
	}
	if _, err := os.Stat(path); err == nil {
		exitIf(fmt.Errorf("%s already exists", path))
	}
	cfg, _ := config.Load(path)
	exitIf(cfg.Save())
	fmt.Println("wrote", path)
}

func runScan(c *kube.Client) int {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	issues, err := c.Scan(ctx)
	exitIf(err)
	red := lipgloss.NewStyle().Foreground(lipgloss.Color("#f7768e")).Bold(true)
	yellow := lipgloss.NewStyle().Foreground(lipgloss.Color("#e0af68")).Bold(true)
	dim := lipgloss.NewStyle().Foreground(lipgloss.Color("#737aa2"))
	fmt.Printf("kubesage scan · context %s · namespace %s\n\n", c.Context, c.NamespaceLabel())
	if len(issues) == 0 {
		fmt.Println(lipgloss.NewStyle().Foreground(lipgloss.Color("#9ece6a")).Render("✓ no issues detected"))
		return 0
	}
	code := 0
	for _, is := range issues {
		icon := yellow.Render("▲ WARN ")
		if is.Severity == kube.HealthErr {
			icon, code = red.Render("✗ ERROR"), 2
		}
		obj := is.Kind + "/" + is.Name
		if is.Namespace != "" {
			obj = is.Namespace + "/" + obj
		}
		fmt.Printf("%s %-18s %s\n        %s\n", icon, is.Reason, obj, is.Message)
		if is.Hint != "" {
			fmt.Println(dim.Render("        → " + is.Hint))
		}
	}
	return code
}

func runAsk(cfg *config.Config, c *kube.Client, q string, readOnly bool) {
	if strings.TrimSpace(q) == "" {
		exitIf(fmt.Errorf(`usage: kubesage ask "why is my pod crashing?"`))
	}
	p, ok := cfg.AI.Profiles[cfg.AI.Active]
	if !ok {
		exitIf(fmt.Errorf("AI profile %q not configured", cfg.AI.Active))
	}
	prov, err := ai.New(cfg.AI.Active, p)
	exitIf(err)
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()
	scanCtx, scanCancel := context.WithTimeout(ctx, 30*time.Second)
	issues, _ := c.Scan(scanCtx)
	scanCancel()
	prompt := fmt.Sprintf("Cluster context: kube-context=%q namespace=%q\nDetected issues:\n%s\nQuestion: %s",
		c.Context, c.NamespaceLabel(), kube.FormatIssues(issues), q)

	system := ai.SystemPrompt
	if cfg.AI.AllowCommands {
		system += agent.ToolInstructions
	}
	danger := false
	if re, err := regexp.Compile(cfg.DangerContexts); err == nil && cfg.DangerContexts != "" {
		danger = re.MatchString(c.Context)
	}
	runner := agent.Runner{BaseArgs: c.KubectlArgs(), Timeout: 60 * time.Second}
	in := bufio.NewReader(os.Stdin)
	dim := lipgloss.NewStyle().Foreground(lipgloss.Color("#737aa2"))
	cmdStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("#7aa2f7")).Bold(true)
	warn := lipgloss.NewStyle().Foreground(lipgloss.Color("#e0af68")).Bold(true)

	msgs := []ai.Message{{Role: "user", Content: prompt}}
	for step := 0; ; step++ {
		out := make(chan string, 64)
		errc := make(chan error, 1)
		go func() { errc <- prov.Stream(ctx, system, msgs, out); close(out) }()
		var reply strings.Builder
		for s := range out {
			fmt.Print(s)
			reply.WriteString(s)
		}
		fmt.Println()
		exitIf(<-errc)
		msgs = append(msgs, ai.Message{Role: "assistant", Content: reply.String()})

		cmds := agent.ExtractRun(reply.String())
		if !cfg.AI.AllowCommands || len(cmds) == 0 {
			return
		}
		if step >= cfg.AI.MaxSteps {
			fmt.Println(warn.Render(fmt.Sprintf("step limit (%d) reached", cfg.AI.MaxSteps)))
			return
		}
		var results []agent.Result
		for _, cmd := range cmds {
			fmt.Println(cmdStyle.Render("$ "+cmd.Display()) + dim.Render("   ["+cmd.Risk.String()+"]"))
			switch {
			case cmd.Risk == agent.Blocked:
				fmt.Println(dim.Render("  blocked: " + cmd.Reason))
				results = append(results, agent.Result{Command: cmd})
				continue
			case cmd.Risk == agent.Write && readOnly:
				fmt.Println(dim.Render("  skipped: read-only mode"))
				results = append(results, agent.Result{Command: cmd, Declined: true, Note: "read-only mode"})
				continue
			case cmd.Risk == agent.Write || !cfg.AI.AutoRunReadOnly:
				if !approve(in, c.Context, cmd, danger && cmd.Risk == agent.Write) {
					results = append(results, agent.Result{Command: cmd, Declined: true})
					continue
				}
			}
			outText, err := runner.Run(ctx, cmd)
			fmt.Println(dim.Render(indent(outText, 12)))
			results = append(results, agent.Result{Command: cmd, Output: outText, Err: err})
		}
		msgs = append(msgs, ai.Message{Role: "user", Content: agent.FormatResults(results)})
		fmt.Println()
	}
}

// approve asks on the terminal; production-like contexts require typing "approve".
func approve(in *bufio.Reader, kctx string, cmd agent.Command, typed bool) bool {
	if typed {
		fmt.Printf("  %s is production-like. Type \"approve\" to run (%s): ", kctx, cmd.Reason)
	} else {
		fmt.Printf("  Run on %s? (%s) [y/N] ", kctx, cmd.Reason)
	}
	line, _ := in.ReadString('\n')
	line = strings.TrimSpace(strings.ToLower(line))
	if typed {
		return line == "approve"
	}
	return line == "y" || line == "yes"
}

func indent(s string, maxLines int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > maxLines {
		lines = append(lines[:maxLines], fmt.Sprintf("… (%d more lines)", len(lines)-maxLines))
	}
	return "  │ " + strings.Join(lines, "\n  │ ")
}

func runAI(cfg *config.Config, args []string) {
	if len(args) == 0 || args[0] == "list" {
		fmt.Printf("AI profiles in %s (active: %s)\n\n", cfg.Path(), cfg.AI.Active)
		for _, name := range cfg.ProfileNames() {
			p := cfg.AI.Profiles[name]
			mark := "  "
			if name == cfg.AI.Active {
				mark = "▶ "
			}
			model := p.Model
			if model == "" {
				model = "(auto)"
			}
			fmt.Printf("%s%-10s %-18s %-26s %s\n", mark, name, p.Provider, model, p.KeyStatus())
		}
		fmt.Println("\nSwitch with: kubesage --ai <profile>, ctrl+o in the AI chat, or `active:` in the config file.")
		return
	}
	switch args[0] {
	case "test":
	case "add":
		aiAdd(cfg, args[1:])
		return
	case "set-key", "key":
		if len(args) < 2 {
			exitIf(fmt.Errorf("usage: kubesage ai set-key <profile>   (prompts; or pipe the key on stdin)"))
		}
		aiSetKey(cfg, args[1])
		return
	case "delete-key", "rm-key":
		if len(args) < 2 {
			exitIf(fmt.Errorf("usage: kubesage ai delete-key <profile>"))
		}
		exitIf(cfg.DeleteKey(args[1]))
		exitIf(cfg.Save())
		fmt.Printf("✓ removed the stored key for %s (environment variables are not touched)\n", args[1])
		return
	case "use":
		if len(args) < 2 {
			exitIf(fmt.Errorf("usage: kubesage ai use <profile>"))
		}
		if _, ok := cfg.AI.Profiles[args[1]]; !ok {
			exitIf(fmt.Errorf("no AI profile %q (see `kubesage ai`)", args[1]))
		}
		cfg.AI.Active = args[1]
		exitIf(cfg.Save())
		fmt.Printf("✓ %s is now the default AI profile\n", args[1])
		return
	case "remove", "rm":
		if len(args) < 2 {
			exitIf(fmt.Errorf("usage: kubesage ai remove <profile>"))
		}
		exitIf(cfg.RemoveProfile(args[1]))
		exitIf(cfg.Save())
		fmt.Printf("✓ removed profile %s and its stored key\n", args[1])
		return
	default:
		exitIf(fmt.Errorf("unknown command %q. Use: kubesage ai [list|add|set-key|delete-key|use|remove|test]", args[0]))
	}
	name := cfg.AI.Active
	if len(args) > 1 {
		name = args[1]
	}
	p, ok := cfg.AI.Profiles[name]
	if !ok {
		exitIf(fmt.Errorf("no AI profile %q (see `kubesage ai`)", name))
	}
	prov, err := ai.New(name, p)
	exitIf(err)
	fmt.Printf("testing %s (%s, %s) · %s …\n", name, p.Provider, prov.Model(), p.KeyStatus())
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	out := make(chan string, 64)
	errc := make(chan error, 1)
	start := time.Now()
	go func() {
		errc <- prov.Stream(ctx, "You are a connectivity check.", []ai.Message{{Role: "user", Content: "Reply with exactly: OK"}}, out)
		close(out)
	}()
	var b strings.Builder
	for s := range out {
		b.WriteString(s)
	}
	if err := <-errc; err != nil {
		exitIf(fmt.Errorf("%s failed after %s: %w", name, time.Since(start).Round(time.Millisecond), err))
	}
	reply := strings.TrimSpace(b.String())
	if len(reply) > 120 {
		reply = reply[:120] + "…"
	}
	fmt.Printf("✓ %s answered in %s: %q\n", prov.Model(), time.Since(start).Round(time.Millisecond), reply)
}

// readSecret reads a key without echoing it when stdin is a terminal, or one line from a pipe.
func readSecret(prompt string) string {
	if term.IsTerminal(int(os.Stdin.Fd())) {
		fmt.Fprint(os.Stderr, prompt)
		b, err := term.ReadPassword(int(os.Stdin.Fd()))
		fmt.Fprintln(os.Stderr)
		exitIf(err)
		return strings.TrimSpace(string(b))
	}
	line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
	return strings.TrimSpace(line)
}

func aiSetKey(cfg *config.Config, name string) {
	if _, ok := cfg.AI.Profiles[name]; !ok {
		exitIf(fmt.Errorf("no AI profile %q; create it first with `kubesage ai add %s --provider …`", name, name))
	}
	key := readSecret(fmt.Sprintf("API key for %s (input hidden): ", name))
	where, err := cfg.SetKey(name, key)
	exitIf(err)
	exitIf(cfg.Save())
	p := cfg.AI.Profiles[name]
	fmt.Printf("✓ key for %s stored in %s\n", name, where)
	if p.APIKeyEnv != "" && os.Getenv(p.APIKeyEnv) != "" {
		fmt.Printf("  note: $%s is also set and takes precedence over the stored key\n", p.APIKeyEnv)
	}
	fmt.Printf("  check it with: kubesage ai test %s\n", name)
}

func aiAdd(cfg *config.Config, args []string) {
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		exitIf(fmt.Errorf("usage: kubesage ai add <name> --provider <provider> [--model m] [--base-url u] [--key-env VAR] [--no-key]"))
	}
	name := args[0]
	fs := flag.NewFlagSet("ai add", flag.ExitOnError)
	provider := fs.String("provider", "", "anthropic, openai, azure, gemini, ollama or openai-compatible")
	model := fs.String("model", "", "model or Azure deployment name")
	baseURL := fs.String("base-url", "", "endpoint (Azure resource URL, LM Studio, Groq, OpenRouter…)")
	apiVersion := fs.String("api-version", "", "Azure OpenAI api-version")
	keyEnv := fs.String("key-env", "", "environment variable to read the key from")
	keyCmd := fs.String("key-cmd", "", "command that prints the key (e.g. 1Password `op read …`)")
	noKey := fs.Bool("no-key", false, "do not prompt for a key")
	use := fs.Bool("use", false, "also make it the default profile")
	_ = fs.Parse(args[1:])
	if *provider == "" {
		exitIf(fmt.Errorf("--provider is required (%s)", strings.Join(config.Providers(), ", ")))
	}
	p := config.ProviderDefaults(*provider)
	for dst, v := range map[*string]string{&p.Model: *model, &p.BaseURL: *baseURL, &p.APIVersion: *apiVersion, &p.APIKeyEnv: *keyEnv, &p.APIKeyCmd: *keyCmd} {
		if v != "" {
			*dst = v
		}
	}
	exitIf(cfg.AddProfile(name, p))
	if *use || len(cfg.AI.Profiles) == 1 {
		cfg.AI.Active = name
	}
	fmt.Printf("✓ profile %s: %s %s\n", name, p.Provider, p.Model)
	if !*noKey && *keyCmd == "" && p.NeedsKey() && (p.APIKeyEnv == "" || os.Getenv(p.APIKeyEnv) == "") {
		if key := readSecret(fmt.Sprintf("API key for %s (input hidden, empty to skip): ", name)); key != "" {
			where, err := cfg.SetKey(name, key)
			exitIf(err)
			fmt.Printf("✓ key stored in %s\n", where)
		}
	}
	exitIf(cfg.Save())
	fmt.Printf("  saved to %s · %s\n", cfg.Path(), cfg.AI.Profiles[name].KeyStatus())
}

// splitGlobalFlags separates flags defined on fs (with their values) from everything else,
// so global options work before or after subcommand arguments. "--" ends flag scanning.
func splitGlobalFlags(fs *flag.FlagSet, args []string) (globals, rest []string) {
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			rest = append(rest, args[i+1:]...)
			break
		}
		if !strings.HasPrefix(a, "-") || a == "-" {
			rest = append(rest, a)
			continue
		}
		name, _, hasValue := strings.Cut(strings.TrimLeft(a, "-"), "=")
		f := fs.Lookup(name)
		if f == nil {
			rest = append(rest, a) // a subcommand flag such as --provider
			continue
		}
		globals = append(globals, a)
		if bf, ok := f.Value.(interface{ IsBoolFlag() bool }); ok && bf.IsBoolFlag() {
			continue
		}
		if !hasValue && i+1 < len(args) {
			i++
			globals = append(globals, args[i])
		}
	}
	return globals, rest
}

func runPolicyScan(cfg *config.Config, c *kube.Client) int {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	fs, err := c.PolicyScan(ctx, ui.PolicyOptionsFor(cfg))
	exitIf(err)
	red := lipgloss.NewStyle().Foreground(lipgloss.Color("#f7768e")).Bold(true)
	yellow := lipgloss.NewStyle().Foreground(lipgloss.Color("#e0af68")).Bold(true)
	dim := lipgloss.NewStyle().Foreground(lipgloss.Color("#737aa2"))
	fmt.Printf("\npolicy & security checks · %s · namespace %s\n\n", c.Context, c.NamespaceLabel())
	code, errs, warns := 0, 0, 0
	for _, f := range fs {
		label := dim.Render("  info ")
		switch f.Severity {
		case kube.HealthErr:
			label, code = red.Render("✗ ERROR"), 2
			errs++
		case kube.HealthWarn:
			label = yellow.Render("▲ WARN ")
			warns++
		}
		ctr := ""
		if f.Container != "" {
			ctr = " [" + f.Container + "]"
		}
		fmt.Printf("%s %-24s %s/%s/%s%s\n        %s\n", label, f.Check, f.Namespace, strings.TrimSuffix(f.Kind, "s"), f.Workload, ctr, f.Message)
		fmt.Println(dim.Render("        → " + f.Hint))
	}
	fmt.Printf("\n%d errors · %d warnings · %d findings in total\n", errs, warns, len(fs))
	return code
}

func runReport(cfg *config.Config, kubeconfig string, client *kube.Client, args []string) {
	post := false
	for _, a := range args {
		if a == "--post" || a == "-post" {
			post = true
		}
	}
	clients := []*kube.Client{client}
	if len(cfg.Reports.Contexts) > 0 {
		clients = nil
		for _, name := range client.Contexts() {
			for _, p := range cfg.Reports.Contexts {
				if re, err := regexp.Compile(p); err == nil && re.MatchString(name) {
					c, err := kube.New(kubeconfig, name, "")
					exitIf(err)
					clients = append(clients, c)
					break
				}
			}
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	text := report.Build(ctx, clients, report.Options{}, time.Now())
	if !post {
		fmt.Print(text)
		return
	}
	env := cfg.Reports.WebhookEnv
	url := os.Getenv(env)
	if env == "" || url == "" {
		exitIf(fmt.Errorf("--post needs reports.webhook_env in the config and that environment variable set to the webhook URL"))
	}
	exitIf(report.Post(ctx, url, text))
	fmt.Printf("✓ report on %d cluster(s) posted\n", len(clients))
}

func runSchedule(cfg *config.Config, kubeconfig string, args []string) {
	action, dry := "status", false
	for _, a := range args {
		switch a {
		case "run", "status":
			action = a
		case "--dry-run", "-dry-run":
			dry = true
		}
	}
	if action == "status" {
		dry = true
	}
	exitIf(ui.RunSchedules(cfg, kubeconfig, time.Now(), dry, os.Stdout))
}

func runUpdate(args []string) {
	checkOnly := false
	for _, a := range args {
		if a == "--check" || a == "-check" {
			checkOnly = true
		}
	}
	r, err := update.Latest(context.Background())
	exitIf(err)
	if !update.Newer(version, r.Version) {
		fmt.Printf("✓ kubesage %s is the latest version\n", version)
		return
	}
	fmt.Printf("kubesage %s is available (you have %s): %s\n", r.Version, version, r.URL)
	if checkOnly {
		return
	}
	exe, err := os.Executable()
	exitIf(err)
	if p, err := filepath.EvalSymlinks(exe); err == nil {
		exe = p
	}
	if by := update.ManagedBy(exe); by != "" {
		exitIf(fmt.Errorf("this kubesage was installed by %s; update it there", by))
	}
	fmt.Printf("downloading and verifying %s…\n", update.ArchiveName(r.Version, runtime.GOOS, runtime.GOARCH))
	exitIf(update.Apply(context.Background(), r.Version, exe))
	fmt.Printf("✓ updated %s to %s\n", exe, r.Version)
}

// runComplete prints candidates for shell completion; it never fails loudly.
func runComplete(args []string) {
	if len(args) == 0 {
		return
	}
	switch args[0] {
	case "themes":
		fmt.Println(strings.Join(ui.ThemeNames(), "\n"))
	case "contexts":
		if list, err := kube.KubeContexts(os.Getenv("KUBECONFIG")); err == nil {
			for _, c := range list {
				fmt.Println(c.Name)
			}
		}
	case "namespaces":
		c, err := kube.New("", "", "")
		if err != nil {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if l, err := c.Clientset.CoreV1().Namespaces().List(ctx, metav1.ListOptions{}); err == nil {
			for _, n := range l.Items {
				fmt.Println(n.Name)
			}
		}
	}
}

func approvalsNS(cfg *config.Config) string {
	if cfg.Approvals.Namespace != "" {
		return cfg.Approvals.Namespace
	}
	return "kubesage-approvals"
}

func runApprove(cfg *config.Config, c *kube.Client, args []string) {
	exp := time.Hour
	if cfg.Approvals.ExpireMinutes > 0 {
		exp = time.Duration(cfg.Approvals.ExpireMinutes) * time.Minute
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	ns := approvalsNS(cfg)
	if len(args) == 0 || args[0] == "list" {
		list, err := c.Approvals(ctx, ns, exp)
		exitIf(err)
		if len(list) == 0 {
			fmt.Println("no approval requests in", ns, "on", c.Context)
		}
		for _, a := range list {
			fmt.Printf("%-9s %-28s %s · by %s · %s ago\n", a.Status, a.ID, a.Action, a.Requester, time.Since(a.Created).Round(time.Minute))
		}
		return
	}
	approve := true
	for _, a := range args[1:] {
		if a == "--reject" || a == "-reject" {
			approve = false
		}
	}
	a, err := c.DecideApproval(ctx, ns, args[0], approve, exp)
	exitIf(err)
	fmt.Printf("✓ %s %q (requested by %s)\n", a.Status, a.Action, a.Requester)
}

func runAudit(cfg *config.Config, args []string) {
	path := filepath.Join(filepath.Dir(cfg.Path()), "audit.log")
	if cfg.Path() == "" {
		path = filepath.Join(filepath.Dir(config.DefaultPath()), "audit.log")
	}
	action := "verify"
	if len(args) > 0 {
		action = args[0]
	}
	switch action {
	case "verify":
		n, legacy, err := audit.Verify(path)
		exitIf(err)
		fmt.Printf("✓ %s: %d chained entries intact", path, n)
		if legacy > 0 {
			fmt.Printf(" (%d older entries without a hash)", legacy)
		}
		fmt.Println()
	case "export":
		since := 24 * time.Hour
		for i := 1; i < len(args)-1; i++ {
			if args[i] == "--since" || args[i] == "-since" {
				d, err := time.ParseDuration(args[i+1])
				exitIf(err)
				since = d
			}
		}
		if cfg.AuditLoki == "" {
			exitIf(fmt.Errorf("set audit_loki_url in the config (…/loki/api/v1/push)"))
		}
		entries, err := audit.Read(path)
		exitIf(err)
		headers := map[string]string{}
		if t := os.Getenv("KUBESAGE_LOKI_TOKEN"); t != "" {
			headers["Authorization"] = "Bearer " + t
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		n, err := audit.ExportLoki(ctx, cfg.AuditLoki, entries, time.Now().Add(-since), headers)
		exitIf(err)
		fmt.Printf("✓ pushed %d audit entries to Loki\n", n)
	default:
		exitIf(fmt.Errorf("use: kubesage audit verify | export [--since 24h]"))
	}
}

func runServe(cfg *config.Config, cfgPath, kubeconfig, kctx, namespace string, readOnly bool, args []string) {
	addr, allow, noBrowser := "127.0.0.1:7777", false, false
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--addr", "-addr":
			if i+1 < len(args) {
				addr = args[i+1]
				i++
			}
		case "--lan", "-lan":
			addr = "0.0.0.0:" + strings.TrimPrefix(addr[strings.LastIndex(addr, ":"):], ":")
		case "--allow-changes", "-allow-changes":
			allow = true
		case "--no-browser", "-no-browser":
			noBrowser = true
		}
	}
	ro := readOnly || cfg.ReadOnly || !allow
	ui.WebMode = true
	web.AssetVersion = version
	lipgloss.SetColorProfile(termenv.TrueColor)
	lipgloss.SetHasDarkBackground(true)
	ns := strings.ReplaceAll(namespace, "*all*", "")
	newModel := func(r *http.Request) (tea.Model, error) {
		c, err := config.Load(cfgPath) // each browser tab gets its own copy of the settings
		if err != nil {
			return nil, err
		}
		client, err := kube.New(kubeconfig, strings.Split(kctx, ",")[0], ns)
		if err != nil {
			return nil, err
		}
		if u := web.UserFrom(r); u != nil && cfg.Serve.Impersonate {
			if client, err = client.Impersonate(u.Email, u.Groups); err != nil {
				return nil, err
			}
		}
		if namespace == "*all*" {
			client.Namespace = kube.AllNamespaces
		}
		return ui.New(c, client, ui.Options{Kubeconfig: kubeconfig, Context: kctx, Namespace: strings.ReplaceAll(namespace, "*all*", "all"),
			ReadOnly: ro, ChooseContext: kctx == "" && c.ChooseContext && len(client.Contexts()) > 1, Version: version}), nil
	}
	app := &web.App{Kubeconfig: kubeconfig, DefaultContext: strings.Split(kctx, ",")[0], DefaultNS: ns, ReadOnly: ro,
		Version: version, Favorites: cfg.Favorites, AuditPath: filepath.Join(filepath.Dir(cfgPathOrDefault(cfgPath)), "audit.log")}
	if namespace == "*all*" {
		app.DefaultNS = "all"
	}
	if cfg.DangerContexts != "" {
		app.Danger, _ = regexp.Compile(cfg.DangerContexts)
	}
	app.LogLines = cfg.AI.LogLines
	if cfg.Serve.Contexts != "" {
		re, err := regexp.Compile(cfg.Serve.Contexts)
		exitIf(err)
		app.Contexts = re
		if def := strings.Split(kctx, ",")[0]; def != "" && !re.MatchString(def) {
			exitIf(fmt.Errorf("--context %s is not allowed by serve.contexts", def))
		}
	}
	app.AI = func(kctx string) (ai.Provider, error) {
		p, ok := cfg.AI.Profiles[cfg.AI.Active]
		if !ok {
			return nil, fmt.Errorf("AI profile %q not found", cfg.AI.Active)
		}
		if re := cfg.AI.LocalOnlyContexts; re != "" {
			if m, _ := regexp.MatchString(re, kctx); m && !ai.IsLocal(p.Provider, p.BaseURL) {
				return nil, fmt.Errorf("%s is local-only (ai.local_only_contexts): use a local AI profile", kctx)
			}
		}
		return ai.New(cfg.AI.Active, p)
	}
	app.PromURL = func(kctx string) string {
		if u := cfg.Prometheus.URLs[kctx]; u != "" {
			return u
		}
		return cfg.Prometheus.URLs["*"]
	}
	app.PromToken = cfg.Prometheus.BearerTokenCommand
	app.Owners, app.SLOs = cfg.Owners, cfg.SLOs
	app.SnapshotBase = filepath.Join(filepath.Dir(cfgPathOrDefault(cfgPath)), "snapshots")
	wa := &serve.WebApp{API: app.Handler(), Assets: web.Assets(), Index: web.Index}
	if h, _, err := net.SplitHostPort(addr); err == nil {
		if h == "" {
			h = "0.0.0.0" // --addr :8080 listens on every interface; so do the port-forward proxies
		}
		app.ForwardBind = h
	}
	if o := cfg.Serve.OIDC; o.Issuer != "" {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		auth, err := web.NewOIDC(ctx, web.OIDCConfig{Issuer: o.Issuer, ClientID: o.ClientID, ClientSecret: os.Getenv(o.ClientSecretEnv),
			RedirectURL: o.RedirectURL, AllowedDomains: o.AllowedDomains, AllowedEmails: o.AllowedEmails, GroupsClaim: o.GroupsClaim, SessionHours: o.SessionHours})
		cancel()
		exitIf(err)
		wa.Auth, wa.PublicURL = auth, strings.TrimSuffix(o.RedirectURL, "auth/callback")
		app.Impersonate = cfg.Serve.Impersonate
	} else if cfg.Serve.Impersonate {
		exitIf(fmt.Errorf("serve.impersonate needs serve.oidc: kubesage must know who is signed in"))
	}
	srv, err := serve.StartApp(addr, ro, newModel, wa)
	exitIf(err)
	defer app.StopForwards()
	mode := "read-only"
	if !ro {
		mode = "CHANGES ALLOWED"
	}
	fmt.Printf("kubesage web UI (%s) · open:\n\n  %s\n\n", mode, srv.URL)
	switch {
	case wa.Auth != nil && app.Impersonate:
		fmt.Println("Sign-in is on: everyone acts with their own Kubernetes permissions (impersonation).")
	case wa.Auth != nil:
		fmt.Println("Sign-in is on, but without serve.impersonate everyone acts with YOUR cluster credentials.")
	case !strings.HasPrefix(addr, "127.0.0.1") && !strings.HasPrefix(addr, "localhost"):
		fmt.Println("⚠ listening on your network: anyone with this link can use kubesage with YOUR cluster credentials.")
	}
	fmt.Println("The terminal UI is at /term on the same link. Ctrl+C stops the server.")
	if (strings.HasPrefix(addr, "127.0.0.1") || strings.HasPrefix(addr, "localhost")) && cfg.OpenBrowser && !noBrowser {
		if err := ui.OpenBrowser(srv.URL); err != nil {
			fmt.Println("(could not open a browser: " + err.Error() + "; open the link above)")
		}
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	exitIf(srv.Wait(ctx))
}

func cfgPathOrDefault(p string) string {
	if p != "" {
		return p
	}
	return config.DefaultPath()
}

func runBot(cfg *config.Config, kubeconfig string, def *kube.Client, args []string) {
	addr := ":8090"
	for i := 0; i < len(args)-1; i++ {
		if args[i] == "--addr" || args[i] == "-addr" {
			addr = args[i+1]
		}
	}
	b := &bot.Bot{Owners: cfg.Owners, SlackSecret: []byte(os.Getenv(cfg.Bot.SlackSigningSecretEnv))}
	if t := os.Getenv(cfg.Bot.TeamsTokenEnv); t != "" {
		tok, err := base64.StdEncoding.DecodeString(t)
		exitIf(err)
		b.TeamsToken = tok
	}
	if len(b.SlackSecret) == 0 && len(b.TeamsToken) == 0 {
		exitIf(fmt.Errorf("set bot.slack_signing_secret_env and/or bot.teams_token_env in the config, and export those variables"))
	}
	var mu sync.Mutex
	clients := map[string]*kube.Client{def.Context: def, "": def}
	b.Client = func(name string) (*kube.Client, error) {
		mu.Lock()
		defer mu.Unlock()
		if c, ok := clients[name]; ok {
			return c, nil
		}
		c, err := kube.New(kubeconfig, name, "")
		if err == nil {
			clients[name] = c
		}
		return c, err
	}
	for _, name := range def.Contexts() {
		for _, p := range cfg.Bot.Contexts {
			if re, err := regexp.Compile(p); err == nil && re.MatchString(name) {
				b.Contexts = append(b.Contexts, name)
				break
			}
		}
	}
	fmt.Printf("kubesage bot (read-only) on %s · Slack: POST /slack · Teams: POST /teams · health: GET /healthz\n", addr)
	srv := &http.Server{Addr: addr, Handler: b.Handler(), ReadHeaderTimeout: 10 * time.Second}
	exitIf(srv.ListenAndServe())
}

func runSnapshot(cfg *config.Config, cfgPath string, c *kube.Client, args []string) {
	base := filepath.Join(filepath.Dir(cfgPathOrDefault(cfgPath)), "snapshots")
	ns := c.Namespace
	if ns == "" {
		exitIf(fmt.Errorf("pick a namespace with -n"))
	}
	action := "list"
	if len(args) > 0 {
		action = args[0]
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	switch action {
	case "save":
		name := ""
		if len(args) > 1 {
			name = args[1]
		}
		s, err := c.TakeSnapshot(ctx, ns, name)
		exitIf(err)
		p, err := s.Save(base)
		exitIf(err)
		fmt.Printf("✓ snapshot %s of %s/%s: %d objects → %s\n", s.Name, c.Context, ns, len(s.Objects), p)
	case "list":
		ss, err := kube.ListSnapshots(base, c.Context, ns)
		exitIf(err)
		for _, s := range ss {
			fmt.Printf("%-24s %s  %s objects\n", s.Name, s.Taken.Local().Format("2006-01-02 15:04"), s.Objects[""])
		}
	case "diff":
		if len(args) < 2 {
			exitIf(fmt.Errorf("usage: kubesage snapshot diff <name> -n <namespace>"))
		}
		old, err := kube.LoadSnapshot(base, c.Context, ns, args[1])
		exitIf(err)
		cur, err := c.TakeSnapshot(ctx, ns, "now")
		exitIf(err)
		changes := kube.DiffSnapshots(old, cur)
		for _, ch := range changes {
			fmt.Printf("%-8s %s\n", ch.State, ch.Key)
			if ch.State == "changed" {
				for _, l := range diff.Lines(strings.Split(ch.Before, "\n"), strings.Split(ch.After, "\n"), 1) {
					if l != "@@" && l[0] != ' ' {
						fmt.Println("    " + l)
					}
				}
			}
		}
		fmt.Printf("%d changes since %s\n", len(changes), old.Taken.Local().Format("2006-01-02 15:04"))
		if len(changes) > 0 {
			os.Exit(3)
		}
	default:
		exitIf(fmt.Errorf("use: kubesage snapshot save [name] | list | diff <name>"))
	}
}
