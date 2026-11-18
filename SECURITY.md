# Security policy

kubeweave works with your Kubernetes credentials, so security problems matter. Thank you for reporting them responsibly.

## Reporting a vulnerability

**Please do not open a public issue for security problems.**

Use GitHub's private reporting: go to the repository's **Security** tab → **Report a vulnerability**. Include:

- the kubeweave version (`kubeweave version`) and how you run it (terminal, `kubeweave serve`, `kubeweave bot`, MCP)
- what an attacker needs (network access, a malicious object in the cluster, a crafted kubeconfig, …)
- steps to reproduce, and what happens

You will get an answer within 7 days. Fixes are released as soon as they are ready, and the release notes credit
the reporter unless you prefer otherwise.

## Supported versions

kubeweave is in public beta (0.x). Security fixes go into the latest release only; update with `kubeweave update`,
`brew upgrade kubeweave` or the install script.

## How kubeweave protects you

**What leaves your computer**

- **Kubernetes API calls** go only to the clusters in your kubeconfig, with your credentials.
- **AI requests** go to the AI provider you configured (or to a local model such as Ollama). With
  `ai.redact_secrets: true` (the default) Secret values, environment variables with sensitive names (password, token,
  key, …) and secret-looking values in command output are replaced by `<redacted>` before anything is sent. Commands
  the AI wants to run that could reveal Secrets (`get secret -o yaml`, `describe secret`, `--raw`) always need your
  approval. `ai.local_only_contexts` stops cloud AI for matching clusters entirely.
- **Update check:** release builds ask GitHub for the latest version (no personal data;
  `KUBEWEAVE_NO_UPDATE_CHECK=1` turns it off).
- **Alerts, reports and the bot** post only to webhooks you configure.

There is no telemetry.

**Changes to clusters**

- `--readonly` / `read_only: true` blocks every change, including ones the AI proposes.
- Contexts matching `danger_contexts` need the object's name typed before destructive actions; chaos tests, the dev
  loop, namespace clones and scale-to-zero schedules refuse them.
- Change windows, freezes, required reasons and the two-person approval rule are available for teams.
- Every change is written to a hash-chained audit log; `kubeweave audit verify` detects edited or deleted entries.

**Web UI (`kubeweave serve`)**

- Read-only unless started with `--allow-changes`; listens on 127.0.0.1 unless told otherwise.
- Without sign-in, a random 128-bit token in the printed link is the key. On the first visit it is swapped for an
  HttpOnly, SameSite=Strict cookie and removed from the URL, so it does not end up in history or Referer headers.
- With `serve.oidc`, people sign in with your identity provider (authorization code + PKCE, verified ID token and
  nonce, signed sessions). With `serve.impersonate`, each person acts with their own Kubernetes permissions and the
  audit log records who did what. `serve.contexts` limits which clusters are offered.
- Requests that change anything need a custom header (blocks cross-site form posts); pages are served with a strict
  Content Security Policy (`script-src 'self'`), `frame-ancestors 'none'`, `nosniff` and `no-referrer`.
- **Port-forwards opened from the browser run on their own port** (a separate origin) behind a one-time key, and
  kubeweave's cookies and headers are stripped before requests reach the forwarded app, so a malicious app inside the
  cluster cannot act on kubeweave with your session.
- Shells, `$EDITOR` and plugins are disabled in the browser (they would run on the server's machine). Secret values
  stay masked and Secrets cannot be edited.
- The terminal view at `/term` loads xterm.js from jsDelivr with Subresource Integrity hashes.

**Other**

- The bot verifies Slack's signing secret (with replay protection) and Teams' HMAC, and only ever reads.
- Plugins run commands on your computer: they come from your own config, from a team repository only with
  `team.allow_plugins: true`, or from the catalogue after you see the exact command. Values are passed as separate
  arguments, never through a shell.
- Downloads (`kubeweave update`, the install scripts) are verified against the release's `checksums.txt`.

## Known limitations

- Anyone who can create ConfigMaps in the approvals namespace can forge an approval; restrict that namespace with RBAC.
- Without `serve.oidc` + `serve.impersonate`, everyone with the web link acts with the credentials kubeweave runs with.
  Share the link only with people you would give your kubeconfig to.
- Release binaries are not code-signed (macOS Gatekeeper and Windows SmartScreen warn on browser downloads); install
  with Homebrew, the install script or `go install`, and compare checksums.
- AI answers can be wrong or manipulated by text in logs and objects (prompt injection). kubeweave never runs a change
  the AI proposes without your approval, but read the command before you approve it.

## Security review log

**2026-11 (v0.6.0 public beta)**, a review of the web server, sign-in, AI agent, plugins, bot, MCP server, file
handling and installers. Fixed:

| Severity | Finding | Fix |
|---|---|---|
| High | Port-forwards were proxied under kubeweave's own origin (`/fwd/…`): JavaScript in a forwarded in-cluster app could call the kubeweave API with the user's session, and the first link's token could leak through Referer. | Each forward gets its own port (origin) and a one-time key that is swapped for a cookie; kubeweave cookies and headers are stripped before proxying. |
| High | The AI agent auto-ran `kubectl get secret … -oyaml` (short flag form) and `get secrets.v1 -o yaml` as read-only, which could send Secret values to a cloud AI without approval. | Attached short flags and dotted resource names are recognised; such commands need approval. |
| Medium | Output of auto-run AI commands was not redacted (e.g. environment variables in `get pods -o yaml`). | Secret-looking values in YAML, JSON and key=value output are redacted before they reach the AI. |
| Medium | The session token stayed in the URL (address bar, history, downloads list, Referer) and the page needed an inline script. | Token swapped for an HttpOnly SameSite=Strict cookie on first visit; CSP `script-src 'self'`. |
| Medium | The `/term` page loaded xterm.js from a CDN without integrity checks. | Subresource Integrity hashes pinned. |
| Low | The web UI offered every kubeconfig context. | `serve.contexts` allow-list. |
| Low | Sign-in `back=` accepted `/\host` (an open redirect in some browsers); sign-out worked over GET (cross-site sign-out). | Backslashes and newlines rejected; sign-out is POST only. |
| Low | Catalogue plugins built `sh -c` scripts with `$CONTEXT` inside, so an unusual context name could become shell code. | Values are passed as positional arguments. |
| Low | Snapshot and event-history file names could be `..`. | File-name parts never start with a dot. |
