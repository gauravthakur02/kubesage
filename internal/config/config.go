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
	ReadOnly       bool   `yaml:"read_only"`
	OpenBrowser    bool   `yaml:"open_browser"` // open http://localhost:<port> after a port-forward starts
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

	path string
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
		Theme:          "tokyonight",
		RefreshSeconds: 3,
		OpenBrowser:    true,
		ChooseContext:  true,
		Animations:     true,
		DangerContexts: `(?i)(prod|prd|-p$|live)`,
		Favorites:      []string{"pods", "deployments", "services", "events", "nodes"},
		DebugImage:     "busybox:1.36",
		LiveUpdates:    "watch",
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
	data, err := yaml.Marshal(c)
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
