// Package cli generates shell completions and the man page for the kubeweave command.
package cli

import (
	"fmt"
	"strings"
	"time"
)

// Command is one subcommand with its own arguments.
type Command struct {
	Name, Help string
	Args       []string // words completed after the subcommand
}

// Commands are the kubeweave subcommands.
var Commands = []Command{
	{"scan", "print detected issues (exit 2 on errors)", []string{"--policy"}},
	{"ask", "ask the AI about the cluster", nil},
	{"ai", "manage AI profiles and keys", []string{"test", "add", "set-key", "delete-key", "use", "remove"}},
	{"config", "write or locate the config file", []string{"init", "path"}},
	{"mcp", "serve read-only tools to AI agents over MCP", nil},
	{"report", "cluster summary for Slack/Teams", []string{"--post"}},
	{"schedule", "scale-to-zero schedules", []string{"run", "status", "--dry-run"}},
	{"bot", "read-only Slack/Teams bot", []string{"--addr"}},
	{"snapshot", "save a namespace and compare it later", []string{"save", "list", "diff"}},
	{"approve", "approve a pending production change", []string{"list"}},
	{"audit", "verify or export the audit log", []string{"verify", "export"}},
	{"serve", "the full UI in a web browser", []string{"--addr", "--lan", "--allow-changes", "--no-browser"}},
	{"update", "update kubeweave to the latest release", []string{"--check"}},
	{"completion", "shell completion script", []string{"bash", "zsh", "fish", "powershell"}},
	{"man", "print the man page", nil},
	{"version", "print the version", nil},
	{"help", "show help", nil},
}

// Flags are the global flags; a value of true means the flag takes an argument.
var Flags = []struct {
	Name, Help string
	Value      bool
}{
	{"context", "kube context(s), comma separated", true}, {"n", "namespace (all for all namespaces)", true},
	{"A", "all namespaces", false}, {"r", "resource to open first", true}, {"readonly", "disable every change", false},
	{"kubeconfig", "path to kubeconfig", true}, {"config", "config file", true}, {"ai", "AI profile", true},
	{"theme", "colour theme", true}, {"cmd", "kubeweave command to run at startup", true},
}

func names() string {
	var n []string
	for _, c := range Commands {
		n = append(n, c.Name)
	}
	return strings.Join(n, " ")
}

func flagWords() string {
	var n []string
	for _, f := range Flags {
		n = append(n, "-"+f.Name, "--"+f.Name)
	}
	return strings.Join(n, " ")
}

// Completion returns the completion script for a shell. Contexts and namespaces are completed live
// through the hidden `kubeweave __complete contexts|namespaces` command.
func Completion(shell string) (string, error) {
	switch shell {
	case "bash":
		return bash(), nil
	case "zsh":
		return "#compdef kubeweave\nautoload -U +X bashcompinit && bashcompinit\n" + bash() + "\n", nil
	case "fish":
		return fish(), nil
	case "powershell", "pwsh":
		return powershell(), nil
	}
	return "", fmt.Errorf("unknown shell %q (bash, zsh, fish, powershell)", shell)
}

func bash() string {
	var sub strings.Builder
	for _, c := range Commands {
		if len(c.Args) > 0 {
			fmt.Fprintf(&sub, "    %s) words=%q ;;\n", c.Name, strings.Join(c.Args, " "))
		}
	}
	return `# kubeweave bash completion: source <(kubeweave completion bash)
_kubeweave() {
  local cur prev words cmd i
  cur="${COMP_WORDS[COMP_CWORD]}"
  prev="${COMP_WORDS[COMP_CWORD-1]}"
  case "$prev" in
    -context|--context) COMPREPLY=($(compgen -W "$(kubeweave __complete contexts 2>/dev/null)" -- "$cur")); return ;;
    -n|--n) COMPREPLY=($(compgen -W "all $(kubeweave __complete namespaces 2>/dev/null)" -- "$cur")); return ;;
    -theme|--theme) COMPREPLY=($(compgen -W "$(kubeweave __complete themes 2>/dev/null)" -- "$cur")); return ;;
    -kubeconfig|--kubeconfig|-config|--config) COMPREPLY=($(compgen -f -- "$cur")); return ;;
  esac
  cmd=""
  for ((i=1; i<COMP_CWORD; i++)); do
    case "${COMP_WORDS[i]}" in -*) ;; *) cmd="${COMP_WORDS[i]}"; break ;; esac
  done
  if [[ "$cur" == -* ]]; then COMPREPLY=($(compgen -W "` + flagWords() + `" -- "$cur")); return; fi
  if [[ -z "$cmd" ]]; then COMPREPLY=($(compgen -W "` + names() + `" -- "$cur")); return; fi
  words=""
  case "$cmd" in
` + sub.String() + `  esac
  COMPREPLY=($(compgen -W "$words" -- "$cur"))
}
complete -F _kubeweave kubeweave
`
}

func fish() string {
	var b strings.Builder
	b.WriteString("# kubeweave fish completion: kubeweave completion fish | source\ncomplete -c kubeweave -f\n")
	for _, c := range Commands {
		fmt.Fprintf(&b, "complete -c kubeweave -n __fish_use_subcommand -a %s -d %q\n", c.Name, c.Help)
		for _, a := range c.Args {
			fmt.Fprintf(&b, "complete -c kubeweave -n '__fish_seen_subcommand_from %s' -a %q\n", c.Name, a)
		}
	}
	for _, f := range Flags {
		line := fmt.Sprintf("complete -c kubeweave -o %s -l %s -d %q", f.Name, f.Name, f.Help)
		if f.Value {
			line += " -r"
		}
		switch f.Name {
		case "context":
			line += " -a '(kubeweave __complete contexts 2>/dev/null)'"
		case "n":
			line += " -a '(kubeweave __complete namespaces 2>/dev/null)'"
		case "theme":
			line += " -a '(kubeweave __complete themes 2>/dev/null)'"
		}
		b.WriteString(line + "\n")
	}
	return b.String()
}

func powershell() string {
	var sub strings.Builder
	for _, c := range Commands {
		if len(c.Args) > 0 {
			fmt.Fprintf(&sub, "        '%s' = @('%s')\n", c.Name, strings.Join(c.Args, "','"))
		}
	}
	return `# kubeweave PowerShell completion: kubeweave completion powershell | Out-String | Invoke-Expression
Register-ArgumentCompleter -Native -CommandName kubeweave -ScriptBlock {
    param($wordToComplete, $commandAst, $cursorPosition)
    $words = @($commandAst.CommandElements | ForEach-Object { $_.ToString() })
    $prev = if ($words.Count -ge 2) { $words[-1] } else { '' }
    if ($wordToComplete -ne '' -and $words.Count -ge 2) { $prev = $words[-2] }
    $sub = @{
` + sub.String() + `    }
    $candidates = switch -Regex ($prev) {
        '^--?context$' { kubeweave __complete contexts 2>$null; break }
        '^--?n$' { @('all') + (kubeweave __complete namespaces 2>$null); break }
        '^--?theme$' { kubeweave __complete themes 2>$null; break }
        default {
            $cmd = $words | Select-Object -Skip 1 | Where-Object { $_ -notlike '-*' } | Select-Object -First 1
            if ($wordToComplete -like '-*') { '` + strings.ReplaceAll(flagWords(), " ", "','") + `'.Split(',').Trim("'") }
            elseif ($cmd -and $sub.ContainsKey($cmd)) { $sub[$cmd] }
            else { '` + strings.ReplaceAll(names(), " ", "','") + `'.Split(',').Trim("'") }
        }
    }
    $candidates | Where-Object { $_ -like "$wordToComplete*" } | ForEach-Object {
        [System.Management.Automation.CompletionResult]::new($_, $_, 'ParameterValue', $_)
    }
}
`
}

// Man returns the kubeweave(1) man page in roff.
func Man(version string, now time.Time) string {
	esc := func(s string) string { return strings.ReplaceAll(strings.ReplaceAll(s, `\`, `\e`), "-", `\-`) }
	var b strings.Builder
	fmt.Fprintf(&b, ".TH KUBEWEAVE 1 %q \"kubeweave %s\" \"User Commands\"\n", now.Format("2006-01-02"), version)
	b.WriteString(".SH NAME\nkubeweave \\- a Kubernetes IDE for your terminal, with an AI copilot\n")
	b.WriteString(".SH SYNOPSIS\n.B kubeweave\n[\\fIflags\\fR] [\\fIcommand\\fR] [\\fIargs\\fR]\n")
	b.WriteString(".SH DESCRIPTION\nWithout a command, kubeweave opens the interactive UI: resources, logs, shells, port\\-forwards, Helm, " +
		"health checks and an AI assistant that runs commands only with your approval. Press \\fB?\\fR inside for every key and \\fB:tour\\fR for a tour.\n")
	b.WriteString(".SH COMMANDS\n")
	for _, c := range Commands {
		fmt.Fprintf(&b, ".TP\n.B %s", esc(c.Name))
		if len(c.Args) > 0 {
			fmt.Fprintf(&b, " \\fR[%s]", esc(strings.Join(c.Args, "|")))
		}
		fmt.Fprintf(&b, "\n%s\n", esc(c.Help))
	}
	b.WriteString(".SH OPTIONS\n")
	for _, f := range Flags {
		arg := ""
		if f.Value {
			arg = " \\fIvalue\\fR"
		}
		fmt.Fprintf(&b, ".TP\n.BR \\-%s%s\n%s\n", esc(f.Name), arg, esc(f.Help))
	}
	b.WriteString(".SH FILES\n.TP\n.I ~/.config/kubeweave/config.yaml\nconfiguration (theme, AI profiles, policy rules, schedules, …)\n" +
		".TP\n.I ~/.config/kubeweave/audit.log\nevery change made through kubeweave\n")
	b.WriteString(".SH ENVIRONMENT\n.TP\n.B KUBECONFIG\nkubeconfig path(s)\n.TP\n.B NO_COLOR\ndisable colours in command output\n" +
		".TP\n.B KUBEWEAVE_NO_UPDATE_CHECK\ndo not check GitHub for new releases\n")
	b.WriteString(".SH SEE ALSO\n.BR kubectl (1)\n.PP\nhttps://gauravthakur02.github.io/kubeweave/\n")
	return b.String()
}

// Known reports whether name is a kubeweave subcommand.
func Known(name string) bool {
	for _, c := range Commands {
		if c.Name == name {
			return true
		}
	}
	return false
}

// CommandHelp is the short help of one subcommand (kubeweave <command> --help).
func CommandHelp(name string) string {
	for _, c := range Commands {
		if c.Name == name {
			h := fmt.Sprintf("kubeweave %s: %s\n", c.Name, c.Help)
			if len(c.Args) > 0 {
				h += "\nArguments and options: " + strings.Join(c.Args, ", ") + "\n"
			}
			return h + "\nGlobal flags (--context, -n, -A, --readonly, --config, …) can come before or after the command; `kubeweave help` lists them.\n"
		}
	}
	return "unknown command " + name + "\n"
}
