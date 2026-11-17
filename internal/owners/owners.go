// Package owners works out who owns a Kubernetes object: from your config rules first, then from common labels.
package owners

import (
	"regexp"
	"strings"
)

// Rule maps namespaces or labels to a team.
type Rule struct {
	Team       string `yaml:"team"`
	Namespaces string `yaml:"namespaces,omitempty"` // regex
	Label      string `yaml:"label,omitempty"`      // key=value, e.g. app.kubernetes.io/part-of=checkout
	Contact    string `yaml:"contact,omitempty"`    // e.g. #team-checkout or team@example.com
	// WebhookEnv names the environment variable with the team's Slack/Teams webhook for alerts.
	WebhookEnv string `yaml:"webhook_env,omitempty"`
}

// Owner is the result.
type Owner struct {
	Team, Contact, WebhookEnv string
	Source                    string // "rule", "label owner", …
}

// Labels and annotations that name an owner, in order of preference.
var ownerKeys = []string{"owner", "team", "kubesage.io/owner", "app.kubernetes.io/owner", "app.kubernetes.io/team", "app.kubernetes.io/part-of"}

// Resolve finds the owner of an object in namespace ns with the given labels and annotations.
func Resolve(rules []Rule, ns string, labels, annotations map[string]string) (Owner, bool) {
	for _, r := range rules {
		if r.Namespaces != "" {
			re, err := regexp.Compile(r.Namespaces)
			if err != nil || !re.MatchString(ns) {
				continue
			}
		}
		if r.Label != "" {
			k, v, _ := strings.Cut(r.Label, "=")
			if labels[k] != v {
				continue
			}
		}
		if r.Namespaces == "" && r.Label == "" {
			continue
		}
		return Owner{Team: r.Team, Contact: r.Contact, WebhookEnv: r.WebhookEnv, Source: "rule"}, true
	}
	for _, k := range ownerKeys {
		if v := labels[k]; v != "" {
			return Owner{Team: v, Source: "label " + k}, true
		}
		if v := annotations[k]; v != "" {
			return Owner{Team: v, Source: "annotation " + k}, true
		}
	}
	return Owner{}, false
}
