// Package agent lets the AI run kubectl commands: it extracts commands from
// model replies, classifies their risk, and executes approved ones against the
// active context only.
package agent

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

// Risk says how a command may be executed.
type Risk int

const (
	// Read commands only inspect the cluster and may run without approval.
	Read Risk = iota
	// Write commands change the cluster and always need the user's approval.
	Write
	// Blocked commands are never executed (interactive, long-running or shell syntax).
	Blocked
)

func (r Risk) String() string {
	switch r {
	case Read:
		return "read-only"
	case Write:
		return "changes cluster"
	}
	return "blocked"
}

// Command is one kubectl invocation proposed by the model or picked by the user.
type Command struct {
	Raw    string   // as written, e.g. "kubectl get pods -n shop"
	Args   []string // arguments after "kubectl", without context/kubeconfig flags
	Risk   Risk
	Reason string // why it is Write/Blocked, or a note for Read
}

// Display is the command as it will run (minus the injected --context).
func (c Command) Display() string { return "kubectl " + strings.Join(c.Args, " ") }

var (
	runBlockRe  = regexp.MustCompile("(?s)```run[^\\n]*\\n(.*?)```")
	anyBlockRe  = regexp.MustCompile("(?s)```[a-zA-Z]*[^\\n]*\\n(.*?)```")
	shellMetaRe = regexp.MustCompile("[|;&<>`]|\\$\\(")
)

// ExtractRun returns the commands the model asked to execute now (```run blocks).
func ExtractRun(reply string) []Command {
	return extract(reply, runBlockRe)
}

// ExtractSuggested returns every kubectl command in any fenced block of a reply,
// for the user to pick and run manually.
func ExtractSuggested(reply string) []Command {
	return extract(reply, anyBlockRe)
}

func extract(reply string, re *regexp.Regexp) []Command {
	var out []Command
	seen := map[string]bool{}
	for _, m := range re.FindAllStringSubmatch(reply, -1) {
		for _, line := range joinContinuations(m[1]) {
			line = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "$ "))
			if !strings.HasPrefix(line, "kubectl ") || seen[line] {
				continue
			}
			seen[line] = true
			out = append(out, Parse(line))
		}
	}
	return out
}

func joinContinuations(block string) []string {
	var out []string
	cur := ""
	for _, l := range strings.Split(block, "\n") {
		if strings.HasSuffix(strings.TrimRight(l, " "), "\\") {
			cur += strings.TrimSuffix(strings.TrimRight(l, " "), "\\") + " "
			continue
		}
		out = append(out, cur+l)
		cur = ""
	}
	if cur != "" {
		out = append(out, cur)
	}
	return out
}

// Parse splits and classifies a single kubectl command line.
func Parse(line string) Command {
	line = strings.TrimSpace(line)
	c := Command{Raw: line}
	if shellMetaRe.MatchString(stripQuoted(line)) {
		c.Risk, c.Reason = Blocked, "shell operators (pipes, redirects, ;, &&, $()) are not allowed — one plain kubectl command only"
		return c
	}
	words, err := splitWords(line)
	if err != nil || len(words) == 0 || words[0] != "kubectl" {
		c.Risk, c.Reason = Blocked, "not a kubectl command"
		return c
	}
	c.Args = stripTargetingFlags(words[1:])
	c.Risk, c.Reason = Classify(c.Args)
	return c
}

// stripTargetingFlags removes --context/--kubeconfig/--cluster/--server/--token/--as
// so the AI can only act on the context the user is looking at, as the user.
func stripTargetingFlags(args []string) []string {
	// Every kubectl flag that picks the cluster, the identity or the credentials: the AI always works as the
	// current kube-context, never as someone else.
	banned := map[string]bool{"--context": true, "--kubeconfig": true, "--cluster": true, "--server": true, "-s": true,
		"--token": true, "--as": true, "--as-group": true, "--as-uid": true, "--user": true, "--username": true,
		"--password": true, "--client-certificate": true, "--client-key": true, "--certificate-authority": true,
		"--tls-server-name": true, "--auth-provider": true, "--auth-provider-arg": true, "--exec-command": true}
	bannedBool := map[string]bool{"--insecure-skip-tls-verify": true}
	var out []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		name, _, hasEq := strings.Cut(a, "=")
		if bannedBool[name] {
			continue
		}
		if banned[name] {
			if !hasEq {
				i++ // skip value
			}
			continue
		}
		out = append(out, a)
	}
	return out
}

func verbOf(args []string) (string, []string) {
	var pos []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if strings.HasPrefix(a, "-") {
			// flags with separate values we know about
			if !strings.Contains(a, "=") && flagTakesValue(a) {
				i++
			}
			continue
		}
		pos = append(pos, a)
	}
	if len(pos) == 0 {
		return "", nil
	}
	return pos[0], pos[1:]
}

func flagTakesValue(f string) bool {
	switch f {
	case "-n", "--namespace", "-o", "--output", "-l", "--selector", "-c", "--container", "--tail", "--since",
		"--field-selector", "--sort-by", "-f", "--filename", "--replicas", "--type", "-p", "--patch", "--image",
		"--for", "--timeout", "--to-revision", "--grace-period":
		return true
	}
	return false
}

func hasFlag(args []string, names ...string) bool {
	for _, a := range args {
		n, _, _ := strings.Cut(a, "=")
		for _, want := range names {
			if n == want {
				return true
			}
			// short flags with the value attached: -oyaml, -ojsonpath=…
			if len(want) == 2 && want[0] == '-' && want[1] != '-' && strings.HasPrefix(a, want) && len(a) > 2 && !strings.HasPrefix(a, "--") {
				return true
			}
		}
	}
	return false
}

// mentionsSecrets catches secret, secrets, secrets.v1, secret.v1., secrets/name and comma lists, plus "all"
// with an explicit output (which does not include secrets, but is cheap to be careful about).
func mentionsSecrets(rest []string) bool {
	for _, r := range rest {
		if strings.HasPrefix(r, "-") {
			continue
		}
		for _, part := range strings.Split(strings.ToLower(r), ",") {
			part, _, _ = strings.Cut(part, "/")
			part, _, _ = strings.Cut(part, ".") // secrets.v1, secrets.v1.
			if part == "secret" || part == "secrets" {
				return true
			}
		}
	}
	return false
}

// Classify decides whether args (after "kubectl") are read-only, mutating or blocked.
func Classify(args []string) (Risk, string) {
	verb, rest := verbOf(args)
	switch verb {
	case "":
		return Blocked, "no kubectl subcommand"
	case "port-forward", "proxy", "attach", "edit", "debug", "cp", "plugin", "completion", "alpha", "config", "kustomize":
		return Blocked, "`kubectl " + verb + "` is interactive, long-running or local-config only; use kubeweave's own UI for it"
	case "exec":
		if hasFlag(args, "-it", "-ti", "-i", "--stdin", "-t", "--tty") {
			return Blocked, "interactive exec is not allowed; use kubeweave's shell (s)"
		}
		return Write, "runs a process inside a container"
	case "run":
		return Write, "creates a pod"
	}
	if hasFlag(args, "-w", "--watch", "--watch-only") {
		return Blocked, "watch never terminates"
	}
	if verb == "logs" && hasFlag(args, "-f", "--follow") {
		return Blocked, "follow never terminates"
	}

	if hasFlag(args, "--raw") {
		return Write, "raw API access may reveal secrets — needs your approval"
	}
	switch verb {
	case "get", "describe":
		if mentionsSecrets(rest) && (verb == "describe" || hasFlag(args, "-o", "--output", "--template", "--show-managed-fields")) {
			return Write, "may reveal secret values — needs your approval"
		}
		return Read, ""
	case "logs", "top", "events", "explain", "api-resources", "api-versions", "version", "cluster-info", "diff":
		return Read, ""
	case "auth":
		if len(rest) > 0 && (rest[0] == "can-i" || rest[0] == "whoami") {
			return Read, ""
		}
	case "rollout":
		if len(rest) > 0 && (rest[0] == "status" || rest[0] == "history") {
			return Read, ""
		}
		return Write, "rollout " + strings.Join(rest[:min(1, len(rest))], "")
	case "apply", "create", "replace", "patch", "set", "scale", "autoscale", "annotate", "label", "taint", "expose":
		if isDryRun(args) {
			return Read, "dry run"
		}
		return Write, verb + " modifies resources"
	case "delete":
		return Write, "deletes resources"
	case "cordon", "uncordon", "drain":
		return Write, verb + " changes node scheduling"
	}
	return Write, "unrecognised command — treated as changing the cluster"
}

// isDryRun is true only for --dry-run=server|client (or bare --dry-run); --dry-run=none really applies.
func isDryRun(args []string) bool {
	for _, a := range args {
		name, val, hasEq := strings.Cut(a, "=")
		if name == "--dry-run" {
			return !hasEq || val == "server" || val == "client"
		}
	}
	return false
}

// stripQuoted blanks out quoted sections so metacharacters inside JSON patches don't trip the check.
func stripQuoted(s string) string {
	var b strings.Builder
	var q rune
	for _, r := range s {
		switch {
		case q != 0 && r == q:
			q = 0
		case q != 0:
			b.WriteRune('x')
		case r == '\'' || r == '"':
			q = r
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// splitWords is a minimal POSIX-ish word splitter supporting single and double quotes.
func splitWords(s string) ([]string, error) {
	var words []string
	var cur strings.Builder
	inWord := false
	var q rune
	for _, r := range s {
		switch {
		case q != 0:
			if r == q {
				q = 0
			} else {
				cur.WriteRune(r)
			}
		case r == '\'' || r == '"':
			q, inWord = r, true
		case r == ' ' || r == '\t':
			if inWord {
				words = append(words, cur.String())
				cur.Reset()
				inWord = false
			}
		default:
			cur.WriteRune(r)
			inWord = true
		}
	}
	if q != 0 {
		return nil, fmt.Errorf("unterminated quote")
	}
	if inWord {
		words = append(words, cur.String())
	}
	return words, nil
}

// Runner executes commands against one context.
type Runner struct {
	BaseArgs []string // e.g. --context X --kubeconfig Y
	Timeout  time.Duration
	MaxBytes int
}

// Run executes a non-blocked command and returns combined output (truncated).
func (r Runner) Run(ctx context.Context, c Command) (string, error) {
	if c.Risk == Blocked {
		return "", fmt.Errorf("blocked: %s", c.Reason)
	}
	if _, err := exec.LookPath("kubectl"); err != nil {
		return "", fmt.Errorf("kubectl not found on PATH")
	}
	timeout := r.Timeout
	if timeout == 0 {
		timeout = 60 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	args := append(append([]string{}, r.BaseArgs...), c.Args...)
	cmd := exec.CommandContext(ctx, "kubectl", args...)
	var buf bytes.Buffer
	cmd.Stdout, cmd.Stderr = &buf, &buf
	err := cmd.Run()
	out := buf.String()
	max := r.MaxBytes
	if max == 0 {
		max = 12000
	}
	if len(out) > max {
		out = out[:max/2] + "\n…(output truncated)…\n" + out[len(out)-max/2:]
	}
	if ctx.Err() == context.DeadlineExceeded {
		err = fmt.Errorf("timed out after %s", timeout)
	}
	return out, err
}

// ToolInstructions is appended to the system prompt when command execution is enabled.
const ToolInstructions = `

## Running commands
You can run kubectl commands against the user's current context. To run commands, write a fenced
code block with the language "run", one plain kubectl command per line, e.g.

` + "```run\nkubectl get pods -n shop -o wide\nkubectl logs deploy/api -n shop --tail=80\n```" + `

Then STOP and wait: the outputs are sent back to you in the next message, and you continue from there.
Rules:
- Investigate with read-only commands first (get, describe, logs, top, events, rollout status/history, auth can-i).
  They run immediately. Prefer narrow queries (-n, -l, --tail, -o wide) over dumping everything.
- Commands that change the cluster (apply, patch, scale, rollout restart/undo, delete, set, label, cordon, drain...)
  are shown to the user for approval. Only propose them when they are the fix, and say what they do and the risk first.
- One kubectl command per line. No pipes, redirects, &&, ;, $(), --watch, logs -f, exec -it, port-forward or edit.
- Do not pass --context or --kubeconfig; the current context is used automatically.
- Never request secret values.
- Use ` + "```bash" + ` (not run) for commands you only want to show the user.`

var (
	sensitiveName = regexp.MustCompile(`(?i)(pass(word|wd)?|secret|token|api[_-]?key|private[_-]?key|credential|auth|cookie|session|conn(ection)?[_-]?str|dsn)`)
	yamlEnvName   = regexp.MustCompile(`^(\s*)-?\s*name:\s*"?([^"\s]+)"?\s*$`)
	yamlValue     = regexp.MustCompile(`^(\s*)value:\s*.+$`)
	jsonEnvPair   = regexp.MustCompile(`("name"\s*:\s*"([^"]+)"\s*,\s*"value"\s*:\s*)"(?:[^"\\]|\\.)*"`)
	kvSecretLine  = regexp.MustCompile(`(?i)^(\s*"?[\w.-]*(pass(word|wd)?|secret|token|api[_-]?key|private[_-]?key|credential)[\w.-]*"?\s*[:=]\s*)\S.*$`)
	secretData    = regexp.MustCompile(`^(\s+)([\w.-]+):\s*[A-Za-z0-9+/=]{8,}\s*$`)
)

// RedactOutput removes values that look secret from command output before it is sent to an AI: env values whose
// name looks sensitive (YAML and JSON), key=value / key: value lines with sensitive keys, and the data of Secrets.
func RedactOutput(out string) string {
	jsonOut := jsonEnvPair.ReplaceAllStringFunc(out, func(m string) string {
		sub := jsonEnvPair.FindStringSubmatch(m)
		if sensitiveName.MatchString(sub[2]) {
			return sub[1] + `"<redacted>"`
		}
		return m
	})
	lines := strings.Split(jsonOut, "\n")
	pendingSensitive, inSecretData, dataIndent := false, false, ""
	isSecret := strings.Contains(out, "kind: Secret") || strings.Contains(out, `"kind": "Secret"`)
	for i, l := range lines {
		if m := yamlEnvName.FindStringSubmatch(l); m != nil {
			pendingSensitive = sensitiveName.MatchString(m[2])
			continue
		}
		if pendingSensitive && yamlValue.MatchString(l) {
			lines[i] = yamlValue.ReplaceAllString(l, "${1}value: <redacted>")
			pendingSensitive = false
			continue
		}
		if isSecret {
			trimmed := strings.TrimSpace(l)
			if trimmed == "data:" || trimmed == "stringData:" {
				inSecretData, dataIndent = true, l[:len(l)-len(strings.TrimLeft(l, " "))]
				continue
			}
			if inSecretData {
				ind := l[:len(l)-len(strings.TrimLeft(l, " "))]
				if trimmed != "" && len(ind) <= len(dataIndent) {
					inSecretData = false
				} else if secretData.MatchString(l) || strings.Contains(l, ":") {
					k, _, _ := strings.Cut(trimmed, ":")
					lines[i] = ind + k + ": <redacted>"
					continue
				}
			}
		}
		if kvSecretLine.MatchString(l) {
			lines[i] = kvSecretLine.ReplaceAllString(l, "${1}<redacted>")
		}
	}
	return strings.Join(lines, "\n")
}

// FormatResults renders executed commands for the model, with secret-looking values redacted.
func FormatResults(results []Result) string {
	return formatResults(results, true)
}

// FormatResultsRaw renders results without redaction (ai.redact_secrets: false).
func FormatResultsRaw(results []Result) string {
	return formatResults(results, false)
}

func formatResults(results []Result, redact bool) string {
	var b strings.Builder
	b.WriteString("Command results:\n")
	for _, r := range results {
		fmt.Fprintf(&b, "\n$ %s\n", r.Command.Display())
		switch {
		case r.Declined:
			reason := "the user declined this command"
			if r.Note != "" {
				reason = r.Note
			}
			fmt.Fprintf(&b, "[not executed: %s]\n", reason)
		case r.Command.Risk == Blocked:
			fmt.Fprintf(&b, "[not executed: %s]\n", r.Command.Reason)
		default:
			if r.Err != nil {
				fmt.Fprintf(&b, "[exit error: %v]\n", r.Err)
			}
			out := strings.TrimRight(r.Output, "\n")
			if redact {
				out = RedactOutput(out)
			}
			if out == "" {
				out = "(no output)"
			}
			b.WriteString("```\n" + out + "\n```\n")
		}
	}
	b.WriteString("\nContinue: analyse these results. Run more commands if needed, otherwise give your conclusion.")
	return b.String()
}

// Result is the outcome of one command.
type Result struct {
	Command  Command
	Output   string
	Err      error
	Declined bool
	Note     string // why it was not executed, when Declined
}
