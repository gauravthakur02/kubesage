package config

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// TeamDir is where the team repository is cloned.
func TeamDir(repo string) string {
	dir, err := os.UserCacheDir()
	if err != nil {
		dir = os.TempDir()
	}
	name := regexp.MustCompile(`[^A-Za-z0-9._-]+`).ReplaceAllString(strings.TrimSuffix(repo, ".git"), "_")
	return filepath.Join(dir, "kubesage", "team", name)
}

// SyncTeam clones or fast-forwards the team repository and merges its kubesage.yaml into c.
// On a network failure the last clone is used, and the error says so.
func (c *Config) SyncTeam(ctx context.Context) (string, error) {
	t := c.Team
	if t.Repo == "" {
		return "", nil
	}
	if strings.HasPrefix(t.Repo, "-") || (t.Ref != "" && strings.HasPrefix(t.Ref, "-")) {
		return "", fmt.Errorf("team: invalid repo or ref")
	}
	dir := TeamDir(t.Repo)
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	var syncErr error
	if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
		syncErr = runGit(ctx, "-C", dir, "pull", "--ff-only", "--quiet")
	} else {
		args := []string{"clone", "--depth", "1", "--quiet"}
		if t.Ref != "" {
			args = append(args, "--branch", t.Ref)
		}
		if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
			return "", err
		}
		syncErr = runGit(ctx, append(args, "--", t.Repo, dir)...)
	}
	path := t.Path
	if path == "" {
		path = "kubesage.yaml"
	}
	b, err := os.ReadFile(filepath.Join(dir, filepath.Clean("/" + path)[1:]))
	if err != nil {
		if syncErr != nil {
			return "", fmt.Errorf("team: %v", syncErr)
		}
		return "", fmt.Errorf("team: %s not found in %s", path, t.Repo)
	}
	var team Config
	if err := yaml.Unmarshal(b, &team); err != nil {
		return "", fmt.Errorf("team: %s: %w", path, err)
	}
	summary := c.mergeTeam(&team, dir)
	if syncErr != nil {
		return summary, fmt.Errorf("team: could not update (%v); using the last copy", syncErr)
	}
	return summary, nil
}

func runGit(ctx context.Context, args ...string) error {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("git %s: %s", args[0], strings.TrimSpace(string(out)))
	}
	return nil
}

// mergeTeam adds the team's lists to the local config; local scalar settings win.
func (c *Config) mergeTeam(t *Config, dir string) string {
	if c.local != nil { // merged before: start again from the local settings
		*c = *c.withoutTeam()
		c.local = nil
	}
	c.local = &teamUndo{owners: len(c.Owners), rules: len(c.Rules), schedules: len(c.Schedules), fleet: len(c.Fleet), windows: len(c.ChangeWindows),
		freezes: len(c.Freezes), registries: len(c.Policy.AllowedRegistries), plugins: len(c.Plugins),
		danger: c.DangerContexts, approvals: c.Approvals, reason: c.RequireReason, runbooks: c.Runbooks.Dir}
	var parts []string
	add := func(n int, what string) {
		if n > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", n, what))
		}
	}
	c.Rules = append(c.Rules, t.Rules...)
	add(len(t.Rules), "rules")
	c.Schedules = append(c.Schedules, t.Schedules...)
	add(len(t.Schedules), "schedules")
	c.Fleet = append(c.Fleet, t.Fleet...)
	c.Owners = append(c.Owners, t.Owners...)
	add(len(t.Owners), "owner rules")
	add(len(t.Fleet), "fleet patterns")
	c.ChangeWindows = append(c.ChangeWindows, t.ChangeWindows...)
	c.Freezes = append(c.Freezes, t.Freezes...)
	add(len(t.ChangeWindows)+len(t.Freezes), "change windows/freezes")
	c.Policy.AllowedRegistries = append(c.Policy.AllowedRegistries, t.Policy.AllowedRegistries...)
	if t.DangerContexts != "" && t.DangerContexts != c.DangerContexts {
		if c.DangerContexts == "" {
			c.DangerContexts = t.DangerContexts
		} else {
			c.DangerContexts = "(?:" + c.DangerContexts + ")|(?:" + t.DangerContexts + ")"
		}
		parts = append(parts, "danger contexts")
	}
	if c.Approvals.Contexts == "" && t.Approvals.Contexts != "" {
		c.Approvals = t.Approvals
		parts = append(parts, "approval rule")
	}
	if c.RequireReason == "" {
		c.RequireReason = t.RequireReason
	}
	if c.Runbooks.Dir == "" && t.Runbooks.Dir != "" {
		c.Runbooks.Dir = filepath.Join(dir, filepath.Clean("/" + t.Runbooks.Dir)[1:])
		parts = append(parts, "runbooks")
	}
	// Plugins run commands on this computer: only taken from the team file when explicitly allowed.
	if c.Team.AllowPlugins {
		c.Plugins = append(c.Plugins, t.Plugins...)
		add(len(t.Plugins), "plugins")
	} else if len(t.Plugins) > 0 {
		parts = append(parts, fmt.Sprintf("%d plugins ignored (team.allow_plugins is off)", len(t.Plugins)))
	}
	c.teamSummary = strings.Join(parts, ", ")
	return c.teamSummary
}

// TeamSummary describes what the team repository contributed ("" when none).
func (c *Config) TeamSummary() string { return c.teamSummary }

// teamUndo remembers the local values so Save never writes team settings into the personal config file.
type teamUndo struct {
	rules, schedules, fleet, windows, freezes, registries, plugins, owners int
	danger, reason, runbooks                                               string
	approvals                                                              Approvals
}

func cut[T any](s []T, n int) []T {
	if n < len(s) {
		return append([]T(nil), s[:n]...)
	}
	return s
}

// withoutTeam is a copy of c with the team repository's contributions removed.
func (c *Config) withoutTeam() *Config {
	u := c.local
	if u == nil {
		return c
	}
	cp := *c
	cp.Rules, cp.Schedules, cp.Fleet = cut(c.Rules, u.rules), cut(c.Schedules, u.schedules), cut(c.Fleet, u.fleet)
	cp.ChangeWindows, cp.Freezes, cp.Plugins = cut(c.ChangeWindows, u.windows), cut(c.Freezes, u.freezes), cut(c.Plugins, u.plugins)
	cp.Policy.AllowedRegistries = cut(c.Policy.AllowedRegistries, u.registries)
	cp.Owners = cut(c.Owners, u.owners)
	cp.DangerContexts, cp.Approvals, cp.RequireReason, cp.Runbooks.Dir = u.danger, u.approvals, u.reason, u.runbooks
	return &cp
}
