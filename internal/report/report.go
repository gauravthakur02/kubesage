// Package report builds the cluster summary posted by `kubesage report` (Slack or Teams webhook).
package report

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/gauravthakur02/kubesage/internal/kube"
)

// Options tune the report.
type Options struct {
	CertDays  int // certificates expiring within this many days are listed (default 14)
	MaxIssues int // issues listed per cluster (default 10)
}

// Build summarises each cluster as Markdown: health, top issues and expiring certificates.
func Build(ctx context.Context, clients []*kube.Client, o Options, now time.Time) string {
	if o.CertDays <= 0 {
		o.CertDays = 14
	}
	if o.MaxIssues <= 0 {
		o.MaxIssues = 10
	}
	var b strings.Builder
	fmt.Fprintf(&b, "*kubesage report* · %s\n", now.Format("Mon 2 Jan 2006 15:04"))
	for _, c := range clients {
		fmt.Fprintf(&b, "\n*%s* (namespace %s)\n", c.Context, c.NamespaceLabel())
		s, err := c.Fleet(ctx)
		if err != nil {
			fmt.Fprintf(&b, "✗ unreachable: %v\n", err)
			continue
		}
		icon := map[kube.Health]string{kube.HealthOK: "✅", kube.HealthWarn: "⚠️", kube.HealthErr: "❌"}[s.Health()]
		nodes := "nodes: no access"
		if s.Nodes >= 0 {
			nodes = fmt.Sprintf("nodes %d/%d ready", s.Ready, s.Nodes)
		}
		fmt.Fprintf(&b, "%s %s · %s · pods %d/%d running · %d issues (%d critical) · %d warning events\n",
			icon, s.Version, nodes, s.Running, s.Pods, s.Issues, s.Errors, s.Warnings)
		if issues, err := c.Scan(ctx); err == nil && len(issues) > 0 {
			sort.SliceStable(issues, func(i, j int) bool {
				return issues[i].Severity == kube.HealthErr && issues[j].Severity != kube.HealthErr
			})
			for i, is := range issues {
				if i == o.MaxIssues {
					fmt.Fprintf(&b, "  … and %d more\n", len(issues)-i)
					break
				}
				obj := is.Name
				if is.Namespace != "" {
					obj = is.Namespace + "/" + is.Name
				}
				fmt.Fprintf(&b, "  • %s %s: %s\n", obj, is.Reason, oneLine(is.Message, 120))
			}
		}
		if certs, err := c.Certificates(ctx); err == nil {
			for _, ct := range certs {
				if ct.NotAfter.IsZero() {
					continue
				}
				days := int(ct.NotAfter.Sub(now).Hours() / 24)
				if days <= o.CertDays {
					state := fmt.Sprintf("expires in %d days", days)
					if days < 0 {
						state = "EXPIRED"
					}
					fmt.Fprintf(&b, "  🔒 certificate %s/%s %s\n", ct.Namespace, ct.Secret, state)
				}
			}
		}
	}
	return b.String()
}

func oneLine(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > n {
		s = s[:n-1] + "…"
	}
	return s
}

// Post sends text to a Slack or Microsoft Teams incoming webhook ({"text": …} works for both).
func Post(ctx context.Context, url, text string) error {
	body, _ := json.Marshal(map[string]string{"text": text})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("webhook answered %s", resp.Status)
	}
	return nil
}
