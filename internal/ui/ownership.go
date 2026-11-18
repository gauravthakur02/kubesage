package ui

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/gauravthakur02/kubeweave/internal/kube"
	"github.com/gauravthakur02/kubeweave/internal/owners"
	"github.com/gauravthakur02/kubeweave/internal/report"
)

// routeAlerts posts new problems to the webhook of the team that owns them (owners: rules with webhook_env).
func (a *App) routeAlerts(fresh []kube.Issue, ctxs []string) {
	if len(a.cfg.Owners) == 0 {
		return
	}
	rules, store := a.cfg.Owners, a.cl
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		for i, is := range fresh {
			var labels, annos map[string]string
			if c, ok := store.client(ctxs[i]); ok {
				if r, ok := kube.Lookup(is.Kind); ok {
					if obj, err := c.Get(ctx, r, is.Namespace, is.Name); err == nil {
						labels, annos = obj.GetLabels(), obj.GetAnnotations()
					}
				}
			}
			o, found := owners.Resolve(rules, is.Namespace, labels, annos)
			if !found || o.WebhookEnv == "" {
				continue
			}
			url := os.Getenv(o.WebhookEnv)
			if url == "" {
				continue
			}
			text := fmt.Sprintf("kubeweave alert for %s: %s %s/%s %s: %s", o.Team, ctxs[i], is.Namespace, is.Name, is.Reason, is.Message)
			_ = report.Post(ctx, url, text)
		}
	}()
}
