package config

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestTeamSyncMergeAndSave(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	repo := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", repo, "-c", "user.email=t@example.invalid", "-c", "user.name=t"}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %s", args, out)
		}
	}
	git("init", "-q")
	os.WriteFile(filepath.Join(repo, "kubeweave.yaml"), []byte(`
rules: [{name: owner label, require_labels: [owner]}]
fleet: ['-dev$']
danger_contexts: 'team-prod'
approvals: {contexts: 'team-prod'}
plugins: [{name: x, key: X, command: "echo hi"}]
`), 0o644)
	git("add", ".")
	git("commit", "-qm", "team")
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	c, _ := Load(cfgPath)
	c.Team = Team{Repo: repo}
	sum, err := c.SyncTeam(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Rules) != 1 || len(c.Fleet) != 1 || c.Approvals.Contexts != "team-prod" || len(c.Plugins) != 0 ||
		!strings.Contains(c.DangerContexts, "team-prod") || !strings.Contains(sum, "plugins ignored") {
		t.Fatalf("merge: %s · %+v", sum, c)
	}
	if err := c.Save(); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(cfgPath)
	if strings.Contains(string(b), "owner label") || strings.Contains(string(b), "team-prod") {
		t.Fatalf("team settings written to the personal config:\n%s", b)
	}
	if _, err := c.SyncTeam(context.Background()); err != nil || len(c.Rules) != 1 { // second run: pull, no duplicates
		t.Fatalf("resync: %v, %d rules", err, len(c.Rules))
	}
}
