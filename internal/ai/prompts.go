package ai

import "fmt"

// SystemPrompt frames the model as a Kubernetes troubleshooting assistant.
const SystemPrompt = `You are KubeSage, a senior Site Reliability Engineer embedded in a terminal Kubernetes IDE.
You receive live cluster data (YAML, events, container logs, detected issues) and help the operator
understand and fix problems in their workloads.

How to answer:
- Start with a one-line verdict: what is wrong (or "Looks healthy").
- Then "Root cause" with the evidence you used (quote the relevant event, status field or log line).
- Then "Fix" as numbered steps with exact kubectl commands or YAML patches the operator can apply.
- Mention risk when a command is destructive, and prefer the least disruptive fix first.
- If the data is insufficient, say exactly which command to run to get what you need.
- Be concise. Use short markdown headings, bullet points and fenced code blocks. No preamble.
Values shown as <redacted> were removed on purpose; never ask for secrets.`

// AnalyzePrompt asks the model to troubleshoot one object.
func AnalyzePrompt(kind, namespace, name, contextData string) string {
	return fmt.Sprintf("Analyze this %s %s/%s and tell me whether anything is wrong and how to fix it.\n\n%s",
		kind, namespace, name, contextData)
}

// TriagePrompt asks the model to prioritise a list of detected issues.
func TriagePrompt(scope, issues string) string {
	return fmt.Sprintf(`These issues were detected automatically in %s.
Group related issues, rank them by impact, explain the most likely root cause of each group,
and give the first fix to try for each.

%s`, scope, issues)
}
