// kubesage is a terminal Kubernetes IDE with an AI troubleshooting copilot.
package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"regexp"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"golang.org/x/term"

	"kubesage/internal/agent"
	"kubesage/internal/ai"
	"kubesage/internal/config"
	"kubesage/internal/kube"
	"kubesage/internal/ui"
)

var version = "0.1.0-dev"

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
  kubesage version

Flags:
`

func main() {
	fs := flag.NewFlagSet("kubesage", flag.ExitOnError)
	kubeconfig := fs.String("kubeconfig", "", "path to kubeconfig (default: $KUBECONFIG or ~/.kube/config)")
	kctx := fs.String("context", "", "kube context(s); comma-separate to open each cluster in its own tab (default: current context)")
	ns := fs.String("n", "", "namespace (default: from context); use \"all\" for all namespaces")
	allNS := fs.Bool("A", false, "all namespaces")
	res := fs.String("r", "", "resource to open first, e.g. pods, deploy, nodes")
	readOnly := fs.Bool("readonly", false, "disable every change to the cluster (UI actions and AI commands)")
	cfgPath := fs.String("config", "", "config file (default: "+config.DefaultPath()+")")
	profile := fs.String("ai", "", "AI profile to use (overrides config ai.active)")
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
	if sub == "version" {
		if term.IsTerminal(int(os.Stdout.Fd())) && os.Getenv("NO_COLOR") == "" {
			ui.AnimateLogo(os.Stdout, "version "+version, 1500*time.Millisecond)
		} else {
			fmt.Println(ui.PrintableLogo("version " + version))
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
		choose := *kctx == "" && cfg.ChooseContext && len(client.Contexts()) > 1
		app := ui.New(cfg, client, ui.Options{Kubeconfig: *kubeconfig, Context: contexts[0], Namespace: strings.ReplaceAll(namespace, "*all*", "all"),
			Resource: *res, ReadOnly: *readOnly, ExtraContexts: extra, ChooseContext: choose})
		_, err := tea.NewProgram(app, tea.WithAltScreen()).Run()
		exitIf(err)
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
	fs, err := c.PolicyScan(ctx, kube.PolicyOptions{AllowedRegistries: cfg.Policy.AllowedRegistries, SkipNamespaces: cfg.Policy.SkipNamespaces})
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
