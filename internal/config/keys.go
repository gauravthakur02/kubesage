package config

import (
	"fmt"
	"runtime"
	"sort"
	"strings"
	"sync"

	"github.com/zalando/go-keyring"
)

const keyringService = "kubeweave"

var (
	krMu    sync.Mutex
	krCache = map[string]string{}
)

func keyringName() string {
	switch runtime.GOOS {
	case "darwin":
		return "macOS Keychain"
	case "windows":
		return "Windows Credential Manager"
	}
	return "system keyring"
}

// KeyringName is the human name of the OS secret store.
func KeyringName() string { return keyringName() }

func keyringGet(profile string) (string, error) {
	krMu.Lock()
	defer krMu.Unlock()
	if v, ok := krCache[profile]; ok {
		return v, nil
	}
	v, err := keyring.Get(keyringService, "ai-profile:"+profile)
	if err == nil {
		krCache[profile] = v
	}
	return v, err
}

// SetKey stores the API key for a profile. It prefers the OS keychain; if that is not
// available (e.g. a headless Linux box) it falls back to the config file, which is
// written with 0600 permissions. It returns where the key was stored. The caller saves the config.
func (c *Config) SetKey(profile, key string) (string, error) {
	p, ok := c.AI.Profiles[profile]
	if !ok {
		return "", fmt.Errorf("no AI profile %q", profile)
	}
	key = strings.TrimSpace(key)
	if key == "" {
		return "", fmt.Errorf("empty key")
	}
	krMu.Lock()
	err := keyring.Set(keyringService, "ai-profile:"+profile, key)
	if err == nil {
		krCache[profile] = key
	}
	krMu.Unlock()
	if err == nil {
		p.KeyInKeyring, p.APIKey = true, ""
		c.AI.Profiles[profile] = p
		return keyringName(), nil
	}
	p.KeyInKeyring, p.APIKey = false, key
	c.AI.Profiles[profile] = p
	return "config file " + c.Path() + " (0600; " + keyringName() + " unavailable: " + err.Error() + ")", nil
}

// DeleteKey removes a stored key (keychain and config file). Environment variables are untouched.
func (c *Config) DeleteKey(profile string) error {
	p, ok := c.AI.Profiles[profile]
	if !ok {
		return fmt.Errorf("no AI profile %q", profile)
	}
	krMu.Lock()
	delete(krCache, profile)
	err := keyring.Delete(keyringService, "ai-profile:"+profile)
	krMu.Unlock()
	if err != nil && err != keyring.ErrNotFound && p.KeyInKeyring {
		return err
	}
	p.KeyInKeyring, p.APIKey = false, ""
	c.AI.Profiles[profile] = p
	return nil
}

// ProviderDefaults gives a sensible model / env var / base URL per provider.
func ProviderDefaults(provider string) AIProfile {
	switch strings.ToLower(provider) {
	case "anthropic":
		return AIProfile{Provider: "anthropic", Model: "claude-opus-5-5", APIKeyEnv: "ANTHROPIC_API_KEY"}
	case "openai":
		return AIProfile{Provider: "openai", Model: "gpt-4o-mini", APIKeyEnv: "OPENAI_API_KEY"}
	case "gemini":
		return AIProfile{Provider: "gemini", Model: "gemini-2.0-flash", APIKeyEnv: "GEMINI_API_KEY"}
	case "azure":
		return AIProfile{Provider: "azure", APIKeyEnv: "AZURE_OPENAI_API_KEY", APIVersion: "2024-10-21"}
	case "ollama":
		return AIProfile{Provider: "ollama", BaseURL: "http://localhost:11434"}
	case "openai-compatible":
		return AIProfile{Provider: "openai-compatible", BaseURL: "http://localhost:1234/v1"}
	}
	return AIProfile{Provider: provider}
}

// Providers lists the supported provider names.
func Providers() []string {
	p := []string{"anthropic", "openai", "azure", "gemini", "ollama", "openai-compatible"}
	sort.Strings(p)
	return p
}

// AddProfile validates and inserts a profile (overwrites an existing one with the same name).
func (c *Config) AddProfile(name string, p AIProfile) error {
	name = strings.TrimSpace(name)
	if name == "" || strings.ContainsAny(name, " \t:/") {
		return fmt.Errorf("profile name must be a single word, got %q", name)
	}
	switch strings.ToLower(p.Provider) {
	case "anthropic", "openai", "azure", "gemini", "ollama", "openai-compatible":
	default:
		return fmt.Errorf("unknown provider %q (use %s)", p.Provider, strings.Join(Providers(), ", "))
	}
	if p.Provider == "azure" && (p.BaseURL == "" || p.Model == "") {
		return fmt.Errorf("azure needs --base-url https://<resource>.openai.azure.com and --model <deployment>")
	}
	if c.AI.Profiles == nil {
		c.AI.Profiles = map[string]AIProfile{}
	}
	p.Name = name
	c.AI.Profiles[name] = p
	return nil
}

// RemoveProfile deletes a profile and its stored key.
func (c *Config) RemoveProfile(name string) error {
	if _, ok := c.AI.Profiles[name]; !ok {
		return fmt.Errorf("no AI profile %q", name)
	}
	_ = c.DeleteKey(name)
	delete(c.AI.Profiles, name)
	if c.AI.Active == name {
		c.AI.Active = ""
		for _, n := range c.ProfileNames() {
			c.AI.Active = n
			break
		}
	}
	return nil
}
