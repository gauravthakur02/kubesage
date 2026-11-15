package config

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/zalando/go-keyring"
)

func TestKeyResolutionOrder(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses sh")
	}
	p := AIProfile{Provider: "anthropic", APIKeyEnv: "KS_TEST_KEY", APIKeyCmd: "echo from-cmd", APIKey: "from-file"}
	t.Setenv("KS_TEST_KEY", "from-env")
	if k, src := p.ResolveKey(); k != "from-env" || src != "env KS_TEST_KEY" {
		t.Fatalf("env should win: %q %q", k, src)
	}
	os.Unsetenv("KS_TEST_KEY")
	if k, src := p.ResolveKey(); k != "from-cmd" || src != "api_key_cmd" {
		t.Fatalf("cmd should be next: %q %q", k, src)
	}
	p.APIKeyCmd = "exit 1"
	if k, _ := p.ResolveKey(); k != "from-file" {
		t.Fatalf("literal key is the fallback: %q", k)
	}
	p.APIKey = ""
	if s := p.KeyStatus(); !strings.Contains(s, "set $KS_TEST_KEY") || strings.Contains(s, "from") {
		t.Fatalf("status should explain what to set and never show keys: %q", s)
	}
	if s := (AIProfile{Provider: "ollama"}).KeyStatus(); !strings.Contains(s, "no key needed") {
		t.Fatal(s)
	}
}

func TestLoadKeepsDefaultsAndSavesPrivately(t *testing.T) {
	path := filepath.Join(t.TempDir(), "c.yaml")
	os.WriteFile(path, []byte("theme: nord\nai:\n  active: claude\n  profiles:\n    claude: {provider: anthropic, api_key_env: X}\n"), 0o600)
	c, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if c.Theme != "nord" || !c.OpenBrowser || !c.AI.AllowCommands || c.AI.MaxSteps != 6 || len(c.AI.Profiles) != 1 {
		t.Fatalf("unexpected %+v", c)
	}
	if err := c.Save(); err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o600 {
		t.Fatalf("config must be private, got %v", fi.Mode().Perm())
	}
}

func TestSetKeyKeyringAndProfiles(t *testing.T) {
	path := filepath.Join(t.TempDir(), "c.yaml")
	c, _ := Load(path)
	if err := c.AddProfile("claude-fast", AIProfile{Provider: "anthropic", Model: "claude-haiku-5-5"}); err != nil {
		t.Fatal(err)
	}
	if err := c.AddProfile("bad name", AIProfile{Provider: "anthropic"}); err == nil {
		t.Fatal("names with spaces must be rejected")
	}
	if err := c.AddProfile("x", AIProfile{Provider: "nope"}); err == nil {
		t.Fatal("unknown providers must be rejected")
	}
	where, err := c.SetKey("claude-fast", "  sk-ant-secret  ")
	if err != nil || where != KeyringName() {
		t.Fatalf("SetKey: %q %v", where, err)
	}
	if err := c.Save(); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	if strings.Contains(string(data), "sk-ant-secret") || !strings.Contains(string(data), "key_in_keyring: true") {
		t.Fatalf("the key must live in the keychain, not the file:\n%s", data)
	}
	// A fresh load finds it again through the keychain.
	krCache = map[string]string{}
	c2, _ := Load(path)
	if k, src := c2.AI.Profiles["claude-fast"].ResolveKey(); k != "sk-ant-secret" || src != KeyringName() {
		t.Fatalf("reload: %q from %q", k, src)
	}
	if err := c2.DeleteKey("claude-fast"); err != nil {
		t.Fatal(err)
	}
	if k := c2.AI.Profiles["claude-fast"].Key(); k != "" {
		t.Fatal("key should be gone")
	}
	if err := c2.RemoveProfile("claude-fast"); err != nil || c2.AI.Profiles["claude-fast"].Provider != "" {
		t.Fatal("profile should be removed")
	}
}

func TestSetKeyFallsBackToFile(t *testing.T) {
	keyring.MockInitWithError(fmt.Errorf("no secret service"))
	defer keyring.MockInit()
	krCache = map[string]string{}
	c, _ := Load(filepath.Join(t.TempDir(), "c.yaml"))
	where, err := c.SetKey("openai", "sk-x")
	if err != nil || !strings.Contains(where, "config file") || c.AI.Profiles["openai"].APIKey != "sk-x" {
		t.Fatalf("fallback: %q %v", where, err)
	}
}
