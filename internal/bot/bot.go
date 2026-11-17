// Package bot answers read-only questions about clusters from Slack slash commands and Microsoft Teams outgoing
// webhooks: status, issues, owner, help. Requests are verified with Slack's signing secret or Teams' HMAC token.
package bot

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gauravthakur02/kubesage/internal/kube"
	"github.com/gauravthakur02/kubesage/internal/owners"
)

// Bot holds what the answers need.
type Bot struct {
	Client      func(kctx string) (*kube.Client, error) // "" = default context
	Contexts    []string                                // contexts `status` reports on
	Owners      []owners.Rule
	SlackSecret []byte // Slack app signing secret
	TeamsToken  []byte // Teams outgoing-webhook security token (base64-decoded)
	Now         func() time.Time
}

// Handler serves POST /slack, POST /teams and GET /healthz.
func (b *Bot) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /slack", b.slack)
	mux.HandleFunc("POST /teams", b.teams)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("ok")) })
	return mux
}

func (b *Bot) now() time.Time {
	if b.Now != nil {
		return b.Now()
	}
	return time.Now()
}

// ---------- Slack ----------

// VerifySlack checks X-Slack-Signature (v0=HMAC-SHA256 of "v0:<timestamp>:<body>") and rejects replays older than 5 minutes.
func VerifySlack(secret []byte, ts, sig string, body []byte, now time.Time) bool {
	t, err := strconv.ParseInt(ts, 10, 64)
	if err != nil || len(secret) == 0 || now.Sub(time.Unix(t, 0)).Abs() > 5*time.Minute {
		return false
	}
	m := hmac.New(sha256.New, secret)
	fmt.Fprintf(m, "v0:%s:%s", ts, body)
	want := "v0=" + hex.EncodeToString(m.Sum(nil))
	return hmac.Equal([]byte(want), []byte(sig))
}

var slackResponseHost = regexp.MustCompile(`^https://hooks\.slack(-gov)?\.com/`)

func (b *Bot) slack(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 64<<10))
	if err != nil || !VerifySlack(b.SlackSecret, r.Header.Get("X-Slack-Request-Timestamp"), r.Header.Get("X-Slack-Signature"), body, b.now()) {
		http.Error(w, "invalid signature", http.StatusUnauthorized)
		return
	}
	form, _ := url.ParseQuery(string(body))
	text, respURL := form.Get("text"), form.Get("response_url")
	w.Header().Set("Content-Type", "application/json")
	if !slackResponseHost.MatchString(respURL) { // answer inline (and never call an arbitrary URL)
		ctx, cancel := context.WithTimeout(r.Context(), 2500*time.Millisecond)
		defer cancel()
		_ = json.NewEncoder(w).Encode(map[string]string{"response_type": "ephemeral", "text": b.Answer(ctx, text)})
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]string{"response_type": "ephemeral", "text": "⏳ kubesage is checking…"})
	go func() { // Slack wants an answer within 3 seconds; the real one goes to response_url
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		msg, _ := json.Marshal(map[string]string{"response_type": "in_channel", "text": b.Answer(ctx, text)})
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, respURL, bytes.NewReader(msg))
		req.Header.Set("Content-Type", "application/json")
		if resp, err := http.DefaultClient.Do(req); err == nil {
			resp.Body.Close()
		}
	}()
}

// ---------- Teams ----------

// VerifyTeams checks "Authorization: HMAC <base64 HMAC-SHA256 of the body>" with the webhook's security token.
func VerifyTeams(token []byte, auth string, body []byte) bool {
	sig, ok := strings.CutPrefix(auth, "HMAC ")
	if !ok || len(token) == 0 {
		return false
	}
	m := hmac.New(sha256.New, token)
	m.Write(body)
	return hmac.Equal([]byte(base64.StdEncoding.EncodeToString(m.Sum(nil))), []byte(sig))
}

var tags = regexp.MustCompile(`<[^>]*>[^<]*</at>|<[^>]+>`)

func (b *Bot) teams(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 256<<10))
	if err != nil || !VerifyTeams(b.TeamsToken, r.Header.Get("Authorization"), body) {
		http.Error(w, "invalid signature", http.StatusUnauthorized)
		return
	}
	var msg struct {
		Text string `json:"text"`
	}
	_ = json.Unmarshal(body, &msg)
	text := strings.TrimSpace(strings.ReplaceAll(tags.ReplaceAllString(msg.Text, ""), "&nbsp;", " "))
	ctx, cancel := context.WithTimeout(r.Context(), 4500*time.Millisecond) // Teams waits 5 seconds
	defer cancel()
	answer := b.Answer(ctx, text)
	if ctx.Err() != nil {
		answer = "kubesage needed more than 5 seconds; ask about one namespace, e.g. `issues shop`."
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"type": "message", "text": strings.ReplaceAll(answer, "\n", "\n\n")})
}

// ---------- answers ----------

const helpText = "kubesage (read-only):\n" +
	"• `status` — health of every cluster\n" +
	"• `issues <namespace> [@context]` — detected problems\n" +
	"• `changes <namespace> [2h]` — what changed recently\n" +
	"• `owner <namespace>/<deployment>` — who owns a workload\n" +
	"• `help`"

// Answer runs one command.
func (b *Bot) Answer(ctx context.Context, text string) string {
	f := strings.Fields(text)
	if len(f) == 0 {
		return helpText
	}
	kctx := ""
	var args []string
	for _, a := range f[1:] {
		if strings.HasPrefix(a, "@") {
			kctx = strings.TrimPrefix(a, "@")
		} else {
			args = append(args, a)
		}
	}
	switch strings.ToLower(f[0]) {
	case "status":
		return b.status(ctx)
	case "issues", "problems":
		if len(args) == 0 {
			return "which namespace? e.g. `issues shop`"
		}
		return b.issues(ctx, kctx, args[0])
	case "changes", "changed":
		if len(args) == 0 {
			return "which namespace? e.g. `changes shop 2h`"
		}
		since := "2h"
		if len(args) > 1 {
			since = args[1]
		}
		return b.changes(ctx, kctx, args[0], since)
	case "owner", "who":
		if len(args) == 0 {
			return "e.g. `owner shop/web`"
		}
		return b.owner(ctx, kctx, args[len(args)-1])
	}
	return helpText
}

func (b *Bot) status(ctx context.Context) string {
	ctxs := b.Contexts
	if len(ctxs) == 0 {
		ctxs = []string{""}
	}
	var out []string
	for _, n := range ctxs {
		c, err := b.Client(n)
		if err != nil {
			out = append(out, "✗ "+n+": "+err.Error())
			continue
		}
		s, err := c.Fleet(ctx)
		if err != nil {
			out = append(out, "✗ "+c.Context+": unreachable")
			continue
		}
		icon := map[kube.Health]string{kube.HealthOK: "✅", kube.HealthWarn: "⚠️", kube.HealthErr: "❌"}[s.Health()]
		nodes := "nodes ?"
		if s.Nodes >= 0 {
			nodes = fmt.Sprintf("nodes %d/%d", s.Ready, s.Nodes)
		}
		out = append(out, fmt.Sprintf("%s *%s* %s · %s · pods %d/%d · %d issues (%d critical)", icon, c.Context, s.Version, nodes, s.Running, s.Pods, s.Issues, s.Errors))
	}
	return strings.Join(out, "\n")
}

func (b *Bot) issues(ctx context.Context, kctx, ns string) string {
	c, err := b.Client(kctx)
	if err != nil {
		return "✗ " + err.Error()
	}
	is, err := c.WithNamespace(ns).Scan(ctx)
	if err != nil {
		return "✗ " + err.Error()
	}
	if len(is) == 0 {
		return fmt.Sprintf("✅ no issues in %s on %s", ns, c.Context)
	}
	sort.SliceStable(is, func(i, j int) bool { return is[i].Severity == kube.HealthErr && is[j].Severity != kube.HealthErr })
	lines := []string{fmt.Sprintf("%d issues in %s on %s:", len(is), ns, c.Context)}
	for i, x := range is {
		if i == 15 {
			lines = append(lines, fmt.Sprintf("… and %d more", len(is)-i))
			break
		}
		mark := "⚠️"
		if x.Severity == kube.HealthErr {
			mark = "❌"
		}
		lines = append(lines, fmt.Sprintf("%s %s %s: %s", mark, x.Name, x.Reason, firstLine(x.Message, 100)))
	}
	return strings.Join(lines, "\n")
}

func (b *Bot) owner(ctx context.Context, kctx, ref string) string {
	ns, name, ok := strings.Cut(ref, "/")
	if !ok {
		return "use namespace/name, e.g. `owner shop/web`"
	}
	c, err := b.Client(kctx)
	if err != nil {
		return "✗ " + err.Error()
	}
	for _, kind := range []string{"deployments", "statefulsets", "daemonsets", "cronjobs", "services"} {
		r, _ := kube.Lookup(kind)
		obj, err := c.Get(ctx, r, ns, name)
		if err != nil {
			continue
		}
		o, found := owners.Resolve(b.Owners, ns, obj.GetLabels(), obj.GetAnnotations())
		if !found {
			return fmt.Sprintf("%s %s/%s has no owner label; add `owner: <team>` or an `owners:` rule", strings.TrimSuffix(kind, "s"), ns, name)
		}
		s := fmt.Sprintf("%s %s/%s is owned by *%s* (%s)", strings.TrimSuffix(kind, "s"), ns, name, o.Team, o.Source)
		if o.Contact != "" {
			s += " · contact " + o.Contact
		}
		return s
	}
	if o, found := owners.Resolve(b.Owners, ns, nil, nil); found {
		return fmt.Sprintf("%s not found, but namespace %s belongs to *%s*", name, ns, o.Team)
	}
	return fmt.Sprintf("%s/%s not found", ns, name)
}

func firstLine(s string, n int) string {
	s, _, _ = strings.Cut(s, "\n")
	if len(s) > n {
		s = s[:n-1] + "…"
	}
	return s
}

func (b *Bot) changes(ctx context.Context, kctx, ns, since string) string {
	c, err := b.Client(kctx)
	if err != nil {
		return "✗ " + err.Error()
	}
	d := kube.ParseSince(since, 2*time.Hour)
	cs, err := c.WithNamespace(ns).Changes(ctx, b.now().Add(-d))
	if err != nil {
		return "✗ " + err.Error()
	}
	if len(cs) == 0 {
		return fmt.Sprintf("nothing changed in %s on %s in the last %s", ns, c.Context, since)
	}
	lines := []string{fmt.Sprintf("%d changes in %s on %s (last %s):", len(cs), ns, c.Context, since)}
	for i, x := range cs {
		if i == 15 {
			lines = append(lines, fmt.Sprintf("… and %d more", len(cs)-i))
			break
		}
		lines = append(lines, fmt.Sprintf("• %s %s %s/%s: %s", x.Time.Local().Format("15:04"), x.Source, x.Kind, x.Name, x.What))
	}
	return strings.Join(lines, "\n")
}
