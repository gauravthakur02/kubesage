package agent

import (
	"context"
	"os"
	"strings"
	"testing"
)

// KUBESAGE_LIVE_CONTEXT=ctx go test ./internal/agent -run Live -v  (read-only)
func TestLiveRunner(t *testing.T) {
	kctx := os.Getenv("KUBESAGE_LIVE_CONTEXT")
	if kctx == "" {
		t.Skip("set KUBESAGE_LIVE_CONTEXT")
	}
	r := Runner{BaseArgs: []string{"--context", kctx}}
	// The model tries to target another context; it must be stripped.
	c := Parse("kubectl get pods -n nsco-services --context some-prod-cluster")
	if c.Risk != Read || strings.Contains(c.Display(), "some-prod") {
		t.Fatalf("unexpected %+v", c)
	}
	out, err := r.Run(context.Background(), c)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	t.Logf("%s →\n%s", c.Display(), firstLines(out, 4))
	if _, err := r.Run(context.Background(), Parse("kubectl get pods | head")); err == nil {
		t.Fatal("blocked command must not run")
	}
}

func firstLines(s string, n int) string {
	l := strings.Split(s, "\n")
	if len(l) > n {
		l = l[:n]
	}
	return strings.Join(l, "\n")
}
