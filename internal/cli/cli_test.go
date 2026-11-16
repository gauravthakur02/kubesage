package cli

import (
	"strings"
	"testing"
	"time"
)

func TestCompletionAndMan(t *testing.T) {
	for _, sh := range []string{"bash", "zsh", "fish", "powershell"} {
		s, err := Completion(sh)
		if err != nil || !strings.Contains(s, "schedule") || !strings.Contains(s, "__complete contexts") {
			t.Fatalf("%s completion: %v", sh, err)
		}
	}
	if _, err := Completion("tcsh"); err == nil {
		t.Fatal("unknown shell accepted")
	}
	m := Man("1.2.3", time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC))
	if !strings.HasPrefix(m, `.TH KUBESAGE 1 "2026-01-02" "kubesage 1.2.3"`) || !strings.Contains(m, `\-\-readonly`) && !strings.Contains(m, `\-readonly`) {
		t.Fatalf("man page:\n%s", m[:200])
	}
}
