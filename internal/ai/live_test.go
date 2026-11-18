package ai

import (
	"os"
	"testing"

	"github.com/gauravthakur02/kubeweave/internal/config"
)

// Run with KUBEWEAVE_LIVE_OLLAMA=1 go test ./internal/ai -run Live -v
func TestLiveOllama(t *testing.T) {
	if os.Getenv("KUBEWEAVE_LIVE_OLLAMA") == "" {
		t.Skip("set KUBEWEAVE_LIVE_OLLAMA=1 to run against a local Ollama")
	}
	p, _ := New("local", config.AIProfile{Provider: "ollama"})
	out := collect(t, p)
	if out == "" {
		t.Fatal("empty response")
	}
	t.Logf("%s answered: %.200s", p.Model(), out)
}
