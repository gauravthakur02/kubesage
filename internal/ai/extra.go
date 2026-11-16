package ai

import (
	"context"
	"fmt"
	"regexp"
	"strings"
)

// Complete runs one prompt and returns the whole answer (no streaming to the UI).
func Complete(ctx context.Context, p Provider, system, prompt string) (string, error) {
	out := make(chan string, 256)
	errc := make(chan error, 1)
	go func() { errc <- p.Stream(ctx, system, []Message{{Role: "user", Content: prompt}}, out); close(out) }()
	var b strings.Builder
	for s := range out {
		b.WriteString(s)
	}
	return b.String(), <-errc
}

// EstimateTokens is a rough count (about 4 characters per token), good enough for cost hints.
func EstimateTokens(s string) int { return (len(s) + 3) / 4 }

// IsLocal reports whether a provider keeps data on this machine (Ollama, or an OpenAI-compatible server on localhost).
func IsLocal(provider, baseURL string) bool {
	if provider == "ollama" {
		return true
	}
	u := strings.ToLower(baseURL)
	return provider == "openai-compatible" && (strings.Contains(u, "://localhost") || strings.Contains(u, "://127.0.0.1") || strings.Contains(u, "://[::1]"))
}

var fence = regexp.MustCompile("(?s)```([a-zA-Z]*)[ \t]*\n(.*?)```")

// ExtractBlock returns the body of the first fenced code block whose language is one of langs ("" matches an untagged block).
func ExtractBlock(reply string, langs ...string) (string, bool) {
	for _, m := range fence.FindAllStringSubmatch(reply, -1) {
		for _, l := range langs {
			if strings.EqualFold(m[1], l) {
				return strings.TrimSpace(m[2]) + "\n", true
			}
		}
	}
	return "", false
}

// FixPrompt asks for the smallest patch that fixes an object.
func FixPrompt(kind, ns, name, manifest, problems string) string {
	return fmt.Sprintf(`Fix this %s %s/%s with the SMALLEST possible change.

Reply with exactly:
1. One fenced block tagged yaml holding a PATCH (not the whole object): only the fields you change, nested from the
   top (e.g. spec.template.spec.containers with each container's name plus the changed fields). It is merged into the
   live object with a strategic merge patch, so lists of containers are matched by name.
2. A line starting with "Why:" and up to 4 bullets explaining the change.

Rules: never change metadata.name/namespace, never touch fields shown as <redacted>, do not invent images or secrets.
If nothing should change, reply "NO CHANGE" and why.

Problems found:
%s

Current object:
%s`, kind, ns, name, problems, manifest)
}

// NarrativePrompt asks for a plain-English account of an incident.
func NarrativePrompt(object, timeline string) string {
	return fmt.Sprintf(`Write a short incident narrative for %s from this timeline (newest first; "(history)" entries come from a local event store).

Format:
- "Summary:" two sentences a manager can read.
- "What happened:" a chronological list (oldest first) with times, merging repeated events.
- "Likely cause:" with the evidence.
- "Follow-ups:" up to 4 concrete actions.
Do not invent events that are not in the timeline.

%s`, object, timeline)
}

// SearchPrompt turns a question into a kubesage filter.
func SearchPrompt(question, resources string) string {
	return fmt.Sprintf(`Turn this request into a JSON object for kubesage and reply with ONLY one fenced json block:
{"resource": "<one of: %s>", "namespace": "<namespace, all, or empty for current>",
 "filter": "<text filter matched against row cells, may be empty>", "label_selector": "<k8s label selector or empty>",
 "min_restarts": <number or 0>, "status": "<status text to match, e.g. CrashLoopBackOff, or empty>",
 "explain": "<one short sentence on what will be shown>"}

Request: %s`, resources, question)
}
