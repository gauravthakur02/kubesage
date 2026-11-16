// Package config loads and saves kubesage settings.
package config

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"
)

// AIProfile describes one LLM backend the user can switch between at runtime.
type AIProfile struct {
	// Provider is one of: ollama, openai, anthropic, gemini, azure, openai-compatible.
	Provider string `yaml:"provider"`
	Model    string `yaml:"model,omitempty"`
	// BaseURL overrides the provider endpoint (LM Studio, vLLM, Groq, OpenRouter, Azure resource URL...).
	BaseURL string `yaml:"base_url,omitempty"`
	// APIKeyEnv names the environment variable holding the key (preferred over APIKey).
	APIKeyEnv string `yaml:"api_key_env,omitempty"`
	// APIKeyCmd is a command that prints the key, e.g. a macOS Keychain or 1Password lookup:
	//   security find-generic-password -s kubesage-anthropic -w
	//   op read op://Private/Anthropic/credential
	APIKeyCmd string `yaml:"api_key_cmd,omitempty"`
	// KeyInKeyring means `kubesage ai set-key` (or :ai-key) stored the key in the OS keychain
	// (macOS Keychain, Windows Credential Manager, Linux Secret Service) under service "kubesage".
	KeyInKeyring bool `yaml:"key_in_keyring,omitempty"`
	// PriceInPerMTok / PriceOutPerMTok (USD per million tokens) turn the token estimate into a cost hint.
	PriceInPerMTok  float64 `yaml:"price_in_per_mtok,omitempty"`
	PriceOutPerMTok float64 `yaml:"price_out_per_mtok,omitempty"`
	// APIKey is a literal key. Prefer APIKeyEnv, APIKeyCmd or the keychain so secrets stay out of config files.
	APIKey string `yaml:"api_key,omitempty"`
	// APIVersion is used by Azure OpenAI.
	APIVersion string `yaml:"api_version,omitempty"`
	MaxTokens  int    `yaml:"max_tokens,omitempty"`

	// Name is filled in from the profiles map key when loading; it is not stored.
	Name string `yaml:"-"`
}

var (
	keyCacheMu sync.Mutex
	keyCache   = map[string]string{}
)

// Key resolves the API key: environment variable, then key command, then literal key.
func (p AIProfile) Key() string {
	k, _ := p.ResolveKey()
	return k
}

// ResolveKey returns the key and a human description of where it came from (never the key itself).
func (p AIProfile) ResolveKey() (key, source string) {
	if p.APIKeyEnv != "" {
		if v := os.Getenv(p.APIKeyEnv); v != "" {
			return v, "env " + p.APIKeyEnv
		}
	}
	if p.APIKeyCmd != "" {
		keyCacheMu.Lock()
		defer keyCacheMu.Unlock()
		if v, ok := keyCache[p.APIKeyCmd]; ok {
			return v, "api_key_cmd"
		}
		if v, err := runKeyCmd(p.APIKeyCmd); err == nil && v != "" {
			keyCache[p.APIKeyCmd] = v
			return v, "api_key_cmd"
		}
	}
	if p.KeyInKeyring && p.Name != "" {
		if v, err := keyringGet(p.Name); err == nil && v != "" {
			return v, keyringName()
		}
	}
	if p.APIKey != "" {
		return p.APIKey, "config file"
	}
	return "", ""
}

func runKeyCmd(cmdline string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		cmd = exec.CommandContext(ctx, "powershell", "-NoProfile", "-Command", cmdline)
	} else {
		cmd = exec.CommandContext(ctx, "sh", "-c", cmdline)
	}
	out, err := cmd.Output()
	return strings.TrimSpace(string(out)), err
}

// NeedsKey reports whether the provider requires an API key.
func (p AIProfile) NeedsKey() bool {
	switch strings.ToLower(p.Provider) {
	case "ollama", "openai-compatible", "lmstudio", "vllm":
		return false
	}
	return true
}

// KeyStatus is a one-line, secret-free description for UIs.
func (p AIProfile) KeyStatus() string {
	_, src := p.ResolveKey()
	switch {
	case src != "":
		return "✓ key from " + src
	case !p.NeedsKey():
		return "✓ no key needed"
	case p.APIKeyEnv != "":
		return "✗ no key: set $" + p.APIKeyEnv
	}
	return "✗ no key configured"
}

// AIConfig groups the AI profiles.
type AIConfig struct {
	Active   string `yaml:"active"`
	Redact   bool   `yaml:"redact_secrets"`
	LogLines int    `yaml:"log_lines"`
	// LocalOnlyContexts is a regex of kube-contexts whose data may only go to a local model (Ollama or localhost).
	LocalOnlyContexts string `yaml:"local_only_contexts,omitempty"`
	// AllowCommands lets the AI run kubectl commands (```run blocks).
	AllowCommands bool `yaml:"allow_commands"`
	// AutoRunReadOnly runs read-only commands without asking; changes always need approval.
	AutoRunReadOnly bool `yaml:"auto_run_read_only"`
	// MaxSteps caps how many command rounds the AI may take per question.
	MaxSteps int                  `yaml:"max_steps"`
	Profiles map[string]AIProfile `yaml:"profiles"`
}

// Config is the root configuration document.
type Config struct {
	Theme          string `yaml:"theme"`
	RefreshSeconds int    `yaml:"refresh_seconds"`
	// SlowRefreshSeconds is how often kubesage's own report tables (certs, images, sizing, …) re-query the cluster (default 30).
	SlowRefreshSeconds int  `yaml:"slow_refresh_seconds,omitempty"`
	ReadOnly           bool `yaml:"read_only"`
	OpenBrowser        bool `yaml:"open_browser"` // open http://localhost:<port> after a port-forward starts
	// ChooseContext shows the cluster chooser at startup when --context is not given.
	ChooseContext bool `yaml:"choose_context_on_start"`
	// Animations turns the animated logo (turning wheel, twinkling sparkle) on or off.
	Animations     bool     `yaml:"animations"`
	DangerContexts string   `yaml:"danger_contexts"`
	Favorites      []string `yaml:"favorites"`
	AI             AIConfig `yaml:"ai"`
	Policy         Policy   `yaml:"policy"`
	// DebugImage is used for ephemeral debug containers in images without a shell.
	// Point it at your own registry if admission policies only allow approved images.
	DebugImage string `yaml:"debug_image"`
	// Views are saved pane layouts (:view save <name>, b to open).
	Views map[string]View `yaml:"views,omitempty"`
	// LiveUpdates is "watch" (instant updates via the Kubernetes watch API, default) or "poll".
	LiveUpdates string `yaml:"live_updates"`
	// Cost prices compute for the right-sizing view (optional).
	Cost Cost `yaml:"cost,omitempty"`
	// Columns adds custom columns per resource (key: resource name such as pods or deployments).
	Columns map[string][]Column `yaml:"columns,omitempty"`
	// Plugins are your own commands bound to a key on selected resources.
	Plugins []Plugin `yaml:"plugins,omitempty"`
	// AuditLog records every change kubesage makes (default on); set false to disable.
	AuditLog *bool `yaml:"audit_log,omitempty"`
	// UpdateCheck looks for a newer kubesage release at startup (default on for release builds).
	UpdateCheck *bool `yaml:"update_check,omitempty"`
	// Watchlist is what the background monitor watches for new problems (:pin / :unpin).
	Watchlist []Pin `yaml:"watchlist,omitempty"`
	// Alerts configures how new problems on the watch list are announced.
	Alerts Alerts `yaml:"alerts,omitempty"`
	// Prometheus enables long-term metrics (right-sizing over days, metric spikes in the timeline).
	Prometheus Prometheus `yaml:"prometheus,omitempty"`
	// Runbooks is a folder (optionally a git clone) of Markdown runbooks the AI and alerts use.
	Runbooks Runbooks `yaml:"runbooks,omitempty"`
	// OpenCost points at OpenCost or Kubecost (auto-detected in the opencost/kubecost namespaces when empty).
	OpenCost OpenCost `yaml:"opencost,omitempty"`
	// SecretRotationDays flags Secrets not changed for longer than this (default 90).
	SecretRotationDays int `yaml:"secret_rotation_days,omitempty"`
	// ChangeWindows restricts when changes may be made to matching contexts.
	ChangeWindows []ChangeWindow `yaml:"change_windows,omitempty"`
	// Freezes block changes to matching contexts between two dates.
	Freezes []Freeze `yaml:"freezes,omitempty"`
	// RequireReason is a regex of contexts where every change asks for a reason (stored in the audit log).
	RequireReason string `yaml:"require_reason,omitempty"`
	// ForwardProfiles are named sets of port-forwards (:fwd save <name>, :fwd <name>).
	ForwardProfiles map[string][]ForwardSpec `yaml:"forward_profiles,omitempty"`
	// Mouse enables clicking rows and headers and wheel scrolling (hold Option/Shift to select text).
	Mouse bool `yaml:"mouse,omitempty"`
	// Keys remaps keys: {original: new}, e.g. {w: g} makes g open the relationships graph.
	Keys map[string]string `yaml:"keys,omitempty"`
	// HideColumns hides table columns per resource, e.g. {pods: [IP, NODE]} (:cols toggles).
	HideColumns map[string][]string `yaml:"hide_columns,omitempty"`
	// TourDone is set after the first-run tour has been shown (:tour shows it again).
	TourDone bool `yaml:"tour_done,omitempty"`
	// Fleet lists kube-contexts (regular expressions) for the fleet view; empty = contexts already connected.
	Fleet []string `yaml:"fleet,omitempty"`
	// EventHistory keeps Kubernetes events on local disk so timelines go back days.
	EventHistory EventHistory `yaml:"event_history,omitempty"`
	// Rules are your own policy checks (policy as code), shown in V/:policy and `kubesage scan --policy`.
	Rules []Rule `yaml:"rules,omitempty"`
	// Schedules scale workloads to zero outside office hours (`kubesage schedule run` from cron, :schedules).
	Schedules []Schedule `yaml:"schedules,omitempty"`
	// PluginIndex is the plugin catalogue URL or file for :plugins (default: the one in the kubesage repository).
	PluginIndex string `yaml:"plugin_index,omitempty"`
	// Loki is where logs are kept beyond the pods' lifetime (:loki).
	Loki Loki `yaml:"loki,omitempty"`
	// Traces opens trace IDs found in logs (:trace) in your tracing UI.
	Traces Traces `yaml:"traces,omitempty"`
	// Approvals require a second person to approve changes on matching contexts.
	Approvals Approvals `yaml:"approvals,omitempty"`
	// Team loads shared settings (rules, schedules, fleet, plugins, runbooks, windows) from a git repository.
	Team Team `yaml:"team,omitempty"`
	// AuditLoki is a Grafana Loki push URL for `kubesage audit export` (…/loki/api/v1/push).
	AuditLoki string `yaml:"audit_loki_url,omitempty"`
	// Reports configures `kubesage report` (summary posted to a Slack/Teams webhook).
	Reports Reports `yaml:"reports,omitempty"`

	path        string
	teamSummary string
	local       *teamUndo // what the config held before the team settings were merged in
}

// EventHistory configures the local event store.
type EventHistory struct {
	Off  bool `yaml:"off,omitempty"`
	Days int  `yaml:"days,omitempty"` // retention, default 7
}

// Rule is a custom policy check on Kubernetes objects.
type Rule struct {
	Name string `yaml:"name"`
	// Kinds are resource names, default deployments, statefulsets, daemonsets.
	Kinds []string `yaml:"kinds,omitempty"`
	// Namespaces is a regex of namespaces the rule applies to (default all except policy.skip_namespaces).
	Namespaces string `yaml:"namespaces,omitempty"`
	// Severity is warn (default) or error.
	Severity           string   `yaml:"severity,omitempty"`
	RequireLabels      []string `yaml:"require_labels,omitempty"`
	RequireAnnotations []string `yaml:"require_annotations,omitempty"`
	// Path is a dotted path into the object; [*] walks lists, e.g. spec.template.spec.containers[*].resources.limits.memory.
	Path string `yaml:"path,omitempty"`
	// Match is a regex every value at Path must match (empty: the value must exist).
	Match string `yaml:"match,omitempty"`
	// Forbid is a regex no value at Path may match.
	Forbid  string `yaml:"forbid,omitempty"`
	Message string `yaml:"message,omitempty"`
}

// Schedule scales matching workloads to zero between Down and Up (local time, HH:MM).
type Schedule struct {
	Name      string `yaml:"name"`
	Context   string `yaml:"context"`            // regex of kube-contexts
	Namespace string `yaml:"namespace"`          // namespace (required)
	Selector  string `yaml:"selector,omitempty"` // label selector; empty = every Deployment and StatefulSet
	Down      string `yaml:"down"`               // e.g. "19:00"
	Up        string `yaml:"up"`                 // e.g. "07:00"
	// Days the schedule is active on (mon..sun); on other days workloads stay down all day. Default mon-fri.
	Days []string `yaml:"days,omitempty"`
	// AllowDanger lets the schedule touch contexts matching danger_contexts (off by default).
	AllowDanger bool `yaml:"allow_danger,omitempty"`
}

// Loki configures log search.
type Loki struct {
	// URL is http(s)://… or service:<ns>/<svc>:<port> (default: look for common Loki services).
	URL string `yaml:"url,omitempty"`
}

// Traces configures trace links.
type Traces struct {
	// URL with {trace_id}, e.g. https://grafana.example.com/explore?left={"queries":[{"query":"{trace_id}"}]} or a Jaeger /trace/{trace_id} URL.
	URL string `yaml:"url,omitempty"`
}

// Approvals configures the two-person rule.
type Approvals struct {
	// Contexts is a regex of kube-contexts where every change needs another person's approval.
	Contexts string `yaml:"contexts,omitempty"`
	// Namespace holds the approval requests as ConfigMaps (default kubesage-approvals).
	Namespace string `yaml:"namespace,omitempty"`
	// ExpireMinutes is how long a request stays valid (default 60).
	ExpireMinutes int `yaml:"expire_minutes,omitempty"`
}

// Team points at a git repository with a shared kubesage.yaml.
type Team struct {
	Repo string `yaml:"repo,omitempty"` // git URL
	Ref  string `yaml:"ref,omitempty"`  // branch or tag (default: the repository default)
	Path string `yaml:"path,omitempty"` // file in the repo (default kubesage.yaml)
	// AllowPlugins also loads the team's plugins (they run commands on your computer).
	AllowPlugins bool `yaml:"allow_plugins,omitempty"`
}

// Reports configures the summary posted by `kubesage report`.
type Reports struct {
	// WebhookEnv names the environment variable holding a Slack or Teams incoming-webhook URL.
	WebhookEnv string `yaml:"webhook_env,omitempty"`
	// Contexts are regexes of kube-contexts to include (default: the current context).
	Contexts []string `yaml:"contexts,omitempty"`
}

// View is a saved layout: a split tree of panes (leaf = one pane).
type View struct {
	Context   string  `yaml:"context,omitempty"`
	Namespace string  `yaml:"namespace,omitempty"`
	Resource  string  `yaml:"resource,omitempty"` // resource name, or "ai" / "pulse"
	Stacked   bool    `yaml:"stacked,omitempty"`
	Ratio     float64 `yaml:"ratio,omitempty"`
	A         *View   `yaml:"a,omitempty"`
	B         *View   `yaml:"b,omitempty"`
}

// Policy configures the workload policy/security checks (V, :policy, kubesage scan --policy).
type Policy struct {
	// AllowedRegistries are regular expressions images must match, e.g. '^[a-z0-9-]+\.azurecr\.io/'.
	// Empty turns the registry check off.
	AllowedRegistries []string `yaml:"allowed_registries"`
	// SkipNamespaces are not checked (system components that legitimately need privileges).
	SkipNamespaces []string `yaml:"skip_namespaces"`
}

// Default returns a config with sensible defaults (local Ollama first).
func Default() *Config {
	return &Config{
		Theme:              "tokyonight",
		RefreshSeconds:     3,
		SlowRefreshSeconds: 30,
		OpenBrowser:        true,
		ChooseContext:      true,
		Animations:         true,
		DangerContexts:     `(?i)(prod|prd|-p$|live)`,
		Favorites:          []string{"pods", "deployments", "services", "events", "nodes"},
		DebugImage:         "busybox:1.36",
		LiveUpdates:        "watch",
		Policy: Policy{SkipNamespaces: []string{"kube-system", "kube-public", "kube-node-lease", "gatekeeper-system",
			"calico-system", "tigera-operator", "azure-workload-identity-system"}},
		AI: AIConfig{
			Active:          "local",
			Redact:          true,
			LogLines:        120,
			AllowCommands:   true,
			AutoRunReadOnly: true,
			MaxSteps:        6,
			Profiles: map[string]AIProfile{
				"local":  {Provider: "ollama", BaseURL: "http://localhost:11434"},
				"claude": {Provider: "anthropic", Model: "claude-opus-5-5", APIKeyEnv: "ANTHROPIC_API_KEY"},
				"openai": {Provider: "openai", Model: "gpt-4o-mini", APIKeyEnv: "OPENAI_API_KEY"},
				"gemini": {Provider: "gemini", Model: "gemini-2.0-flash", APIKeyEnv: "GEMINI_API_KEY"},
				"azure": {Provider: "azure", Model: "<deployment-name>", BaseURL: "https://<resource>.openai.azure.com",
					APIKeyEnv: "AZURE_OPENAI_API_KEY", APIVersion: "2024-10-21"},
			},
		},
	}
}

// DefaultPath is $XDG_CONFIG_HOME/kubesage/config.yaml (~/.config by default),
// or %AppData%\kubesage\config.yaml on Windows.
func DefaultPath() string {
	if runtime.GOOS == "windows" {
		if dir, err := os.UserConfigDir(); err == nil {
			return filepath.Join(dir, "kubesage", "config.yaml")
		}
	}
	dir := os.Getenv("XDG_CONFIG_HOME")
	if dir == "" {
		home, _ := os.UserHomeDir()
		dir = filepath.Join(home, ".config")
	}
	return filepath.Join(dir, "kubesage", "config.yaml")
}

// Load reads the config at path, falling back to defaults when it does not exist.
func Load(path string) (*Config, error) {
	if path == "" {
		path = DefaultPath()
	}
	cfg := Default()
	cfg.path = path
	cfg.nameProfiles()
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return cfg, nil
	}
	if err != nil {
		return nil, err
	}
	loaded := Default()
	loaded.AI.Profiles = nil
	if err := yaml.Unmarshal(data, loaded); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	loaded.path = path
	if loaded.EventHistory.Days <= 0 {
		loaded.EventHistory.Days = 7
	}
	if loaded.SlowRefreshSeconds <= 0 {
		loaded.SlowRefreshSeconds = 30
	}
	if loaded.RefreshSeconds <= 0 {
		loaded.RefreshSeconds = 3
	}
	if loaded.AI.MaxSteps <= 0 {
		loaded.AI.MaxSteps = 6
	}
	if loaded.AI.LogLines <= 0 {
		loaded.AI.LogLines = 120
	}
	if len(loaded.AI.Profiles) == 0 {
		loaded.AI.Profiles = cfg.AI.Profiles
	}
	loaded.nameProfiles()
	if len(loaded.Favorites) == 0 {
		loaded.Favorites = cfg.Favorites
	}
	return loaded, nil
}

func (c *Config) nameProfiles() {
	for n, p := range c.AI.Profiles {
		p.Name = n
		c.AI.Profiles[n] = p
	}
}

// Path returns where this config lives on disk.
func (c *Config) Path() string { return c.path }

// Save writes the config to disk with 0600 permissions.
func (c *Config) Save() error {
	if c.path == "" {
		c.path = DefaultPath()
	}
	if err := os.MkdirAll(filepath.Dir(c.path), 0o755); err != nil {
		return err
	}
	data, err := yaml.Marshal(c.withoutTeam())
	if err != nil {
		return err
	}
	return os.WriteFile(c.path, data, 0o600)
}

// ProfileNames returns the configured AI profile names, sorted.
func (c *Config) ProfileNames() []string {
	names := make([]string, 0, len(c.AI.Profiles))
	for n := range c.AI.Profiles {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// Cost is the price of compute, used to estimate monthly waste.
type Cost struct {
	CPUCoreHour float64 `yaml:"cpu_core_hour"` // e.g. 0.031
	MemGiBHour  float64 `yaml:"mem_gib_hour"`  // e.g. 0.004
	Currency    string  `yaml:"currency"`      // e.g. EUR
}

// Column is a custom table column: a JSONPath-like field path (spec.containers[0].image),
// a label (label: team) or an annotation (annotation: owner).
type Column struct {
	Name       string `yaml:"name"`
	Path       string `yaml:"path,omitempty"`
	Label      string `yaml:"label,omitempty"`
	Annotation string `yaml:"annotation,omitempty"`
}

// Plugin runs a command on the selected resource. Variables: $NAME $NAMESPACE $CONTEXT $KIND $RESOURCE $KUBECONFIG.
type Plugin struct {
	Name       string   `yaml:"name"`
	Key        string   `yaml:"key"`     // single key, e.g. ctrl+g or K
	Scopes     []string `yaml:"scopes"`  // resources (pods, deployments) or "all"
	Command    string   `yaml:"command"` // executable
	Args       []string `yaml:"args,omitempty"`
	Background bool     `yaml:"background,omitempty"` // capture output into the viewer instead of handing over the terminal
	Confirm    *bool    `yaml:"confirm,omitempty"`    // ask before running (default true)
}

// AuditEnabled reports whether the audit log is on.
func (c *Config) AuditEnabled() bool { return c.AuditLog == nil || *c.AuditLog }

// Pin is a watched namespace (Name empty) or workload.
type Pin struct {
	Context   string `yaml:"context"`
	Namespace string `yaml:"namespace"`
	Kind      string `yaml:"kind,omitempty"` // resource name, e.g. deployments
	Name      string `yaml:"name,omitempty"`
}

// Alerts controls notifications for the watch list.
type Alerts struct {
	Bell        *bool `yaml:"bell,omitempty"`         // terminal bell (default on)
	Desktop     *bool `yaml:"desktop,omitempty"`      // macOS/Linux desktop notification (default on)
	CertDays    int   `yaml:"cert_days,omitempty"`    // warn when a certificate expires within this many days (default 14)
	IntervalSec int   `yaml:"interval_sec,omitempty"` // how often to check (default 30)
}

// Prometheus is a Prometheus-compatible endpoint (Prometheus, Thanos, Mimir, Azure Monitor managed Prometheus via a local proxy).
type Prometheus struct {
	// URL per kube-context, or "*" for all, e.g. {"*": "http://localhost:9090"}. "service:<ns>/<svc>:<port>" goes through the API server proxy.
	URLs map[string]string `yaml:"urls,omitempty"`
	// BearerTokenCommand prints a token (e.g. az account get-access-token --resource https://prometheus.monitor.azure.com --query accessToken -o tsv).
	BearerTokenCommand string `yaml:"bearer_token_command,omitempty"`
	// Window for right-sizing peaks, e.g. 7d (default).
	Window string `yaml:"window,omitempty"`
}

// Runbooks configures team runbooks. Each .md file may start with front matter:
//
//	---
//	title: Pod crash loops
//	match: [CrashLoopBackOff, OOMKilled]
//	---
type Runbooks struct {
	Dir string `yaml:"dir,omitempty"`
}

// OpenCost configures the cost API.
type OpenCost struct {
	URL    string `yaml:"url,omitempty"`    // http://… or service:<ns>/<svc>:<port>
	Window string `yaml:"window,omitempty"` // default 7d
}

// ChangeWindow allows changes on matching contexts only on Days during Hours.
type ChangeWindow struct {
	Contexts string `yaml:"contexts"`        // regex, e.g. (?i)prod
	Days     string `yaml:"days,omitempty"`  // Mon-Fri, or Mon,Tue,Thu (default every day)
	Hours    string `yaml:"hours,omitempty"` // 08:00-18:00 (default all day)
	TZ       string `yaml:"tz,omitempty"`    // e.g. Europe/Amsterdam (default local)
	Mode     string `yaml:"mode,omitempty"`  // block (default) or warn
}

// Freeze blocks changes between From and To (YYYY-MM-DD or RFC 3339).
type Freeze struct {
	Contexts string `yaml:"contexts"`
	From     string `yaml:"from"`
	To       string `yaml:"to"`
	Reason   string `yaml:"reason,omitempty"`
}

// ForwardSpec is one saved port-forward.
type ForwardSpec struct {
	Context   string `yaml:"context"`
	Namespace string `yaml:"namespace"`
	Resource  string `yaml:"resource"` // pods, services, deployments…
	Name      string `yaml:"name"`
	Local     int    `yaml:"local"`
	Remote    int    `yaml:"remote"`
}
