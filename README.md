<p align="center">
  <img src="assets/logo-animated.svg" alt="kubesage: Kubernetes IDE for your terminal with an AI copilot" width="640">
</p>

# kubesage

A Kubernetes IDE for the terminal, in the spirit of k9s and Lens, with an AI copilot that
reads your cluster's manifests, events and logs and tells you what is wrong and how to fix it.

- A single static binary. Runs in bash, zsh, PowerShell and Windows Terminal (macOS, Linux, Windows).
- Colour themes using truecolor, which falls back to 256 or 16 colours automatically.
- Works with local LLMs (Ollama, LM Studio, vLLM) or with API keys for Anthropic Claude, OpenAI, Azure OpenAI and Google Gemini.
- A built-in issue detector that needs no AI, for CrashLoopBackOff, OOMKilled, image pull errors, unschedulable pods, stuck rollouts, node pressure and more.

## Install

**macOS: Homebrew** (builds from source on your Mac, so there is no "Apple could not verify" warning):
```bash
brew tap gauravthakur02/kubesage https://github.com/gauravthakur02/kubesage
brew trust gauravthakur02/kubesage   # once: Homebrew 7 asks you to trust third-party taps
brew install kubesage                # later: brew upgrade kubesage
```

**macOS and Linux: install script** (amd64, arm64; Linux also armv7). Downloads the latest release, checks it against
`checksums.txt` and installs to `/usr/local/bin`, or `~/.local/bin` when that is not writable. `curl` does not mark files as
quarantined, so macOS runs it without a warning:
```bash
curl -fsSL https://raw.githubusercontent.com/gauravthakur02/kubesage/main/install.sh | sh
```

**Windows** (amd64, arm64) in PowerShell. Installs to `%LOCALAPPDATA%\Programs\kubesage` and adds it to your user PATH:
```powershell
irm https://raw.githubusercontent.com/gauravthakur02/kubesage/main/install.ps1 | iex
```
Both scripts take `KUBESAGE_VERSION` (e.g. `0.2.0`) and `KUBESAGE_INSTALL_DIR`; `install.sh` also takes `KUBESAGE_DOWNLOAD_URL` for a mirror.

**With Go** (any OS; no warning on macOS either):
```bash
go install github.com/gauravthakur02/kubesage@latest
```

**Linux packages** from the [release page](https://github.com/gauravthakur02/kubesage/releases):
```bash
sudo apt install ./kubesage_<version>_amd64.deb          # Debian, Ubuntu
sudo dnf install ./kubesage-<version>-1.x86_64.rpm       # RHEL, Fedora, SUSE (zypper)
sudo apk add --allow-untrusted ./kubesage_<version>_x86_64.apk   # Alpine
```

**Scoop** (Windows, once the bucket repository is set up):
```bash
scoop bucket add kubesage https://github.com/gauravthakur02/scoop-bucket && scoop install kubesage
```

**After installing:**
```bash
kubesage update                       # download, verify and install the newest release (--check only looks)
source <(kubesage completion bash)    # or zsh / fish / powershell; packages and Homebrew install these for you
man kubesage                          # from packages; otherwise: kubesage man > kubesage.1
```
The first start shows a one-minute tour (`:tour` shows it again).

**Manual download:** every [release](https://github.com/gauravthakur02/kubesage/releases) has `.tar.gz` (macOS, Linux) and `.zip` (Windows)
archives plus `checksums.txt`. Unpack and put `kubesage` on your PATH.

> **macOS: "Apple could not verify kubesage is free of malware".** The release binaries are not notarized by Apple, so macOS
> Gatekeeper blocks them when they are **downloaded with a web browser** (the browser marks them as quarantined). Homebrew, the
> install script and `go install` never trigger this; use one of them. If you downloaded the archive by hand, check it against `checksums.txt`, then clear the quarantine flag on that
> one file (this changes nothing else on your Mac):
> ```bash
> shasum -a 256 kubesage_*_darwin_*.tar.gz      # compare with checksums.txt
> xattr -d com.apple.quarantine ./kubesage
> ```
> Or: try to open it once, then **System Settings → Privacy & Security → "kubesage" was blocked → Open Anyway**.

**With Go:**
```bash
go install github.com/gauravthakur02/kubesage@latest
```

**From source:**
```bash
make build        # → bin/kubesage
make install      # → $GOPATH/bin/kubesage
make snapshot     # every package and archive into dist/ via GoReleaser, nothing published
```

Requires the Go version listed in `go.mod`. `kubectl` is optional. It is used for `describe`, `edit` and debug containers; shells and port-forwards work without it. Everything else talks to the API directly through client-go.

## Use

```bash
kubesage                          # current context & namespace
kubesage --context my-aks -n shop # pick context / namespace
kubesage -A -r deploy             # all namespaces, open deployments
kubesage --readonly               # disable delete/scale/restart/edit
kubesage scan                     # CI-friendly issue report (exit 2 on errors)
kubesage ask "why is checkout-api not ready?"   # one-shot AI answer to stdout
kubesage config init              # write a starter config
```

### Finding any resource

Press **`0`** (or `:resources`) to open the **resource browser**. It lists every resource type the cluster serves, grouped into:

| Group | Resources |
|---|---|
| Workloads | Pods, Deployments, StatefulSets, DaemonSets, ReplicaSets, Jobs, CronJobs, HPAs, PodDisruptionBudgets |
| Network | Services, Ingresses, Endpoints, NetworkPolicies |
| Config & Storage | ConfigMaps, Secrets, PVCs, PersistentVolumes, StorageClasses |
| Access control (RBAC) | ServiceAccounts, Roles, RoleBindings, ClusterRoles, ClusterRoleBindings (wildcard rules and `cluster-admin` bindings are flagged) |
| Cluster | Events, Nodes, Namespaces |
| Custom resources | every CRD on the cluster (cert-manager Certificates, Datadog, Argo, Istio…), with the CRD's own printer columns and a Ready/phase status |
| Other APIs | any other listable API (e.g. metrics, snapshot, gateway APIs) |

Type to filter (`rolebinding`, `cert`, `datadog`), then press `enter`. Commands work as well: `:rb`, `:crb`, `:sa`, `:netpol`, `:pv`, `:sc`, `:pdb`, `:rs`, `:ds`, `:cm`,
or any CRD name or short name (`:certificates`, `:cert`). `:crd` lists the CustomResourceDefinitions, and `enter` on one opens its objects.
Describe, YAML, edit, delete and AI analysis work on every type. The header's tab row starts with `0 ☰ All`, and you can change the numbered tabs with `favorites:` in the config.

### Screens

| Key | Screen |
|---|---|
| `1`-`9`, `tab` | resource tabs (favorites) |
| `:pods`, `:deploy`, `:sts`, `:ds`, `:rs`, `:jobs`, `:cj`, `:svc`, `:ing`, `:cm`, `:secret`, `:pvc`, `:hpa`, `:ev`, `:nodes`, `:ns` | jump to a resource (`:pods kube-system` or `:pods all`) |
| `V` / `:policy` | **policy & security checks** for every workload, with AI fixes |
| `H` / `:pulse` | **Pulse**: node/pod/deployment health bars, CPU/memory requests vs capacity, detected issues |
| `M` / `:helm` / `:charts` | **Helm releases** (values, manifests, history, revision diff, rollback) and the **charts** they use (default values, README, templates) |
| `b` / `:view <name>` | open a **saved view** (`:view save <name>` stores the current tab) |
| `space` | **actions menu** for the selected row: every action that applies, including relationships, timeline, network policies, right-sizing and cluster diff |
| `I` / `:incident` | **AI incident triage**: everything unhealthy, root cause, fixes you approve one by one |
| `ctrl+p` | **command palette**: fuzzy search over screens, resources, saved views, plugins and clusters |
| `:rightsize` `:certs` `:images` `:storage` `:gitops` | right-sizing & cost, TLS expiry, images (Trivy), PVC usage, Argo CD/Flux apps; also under **Insights** in `0` |
| `:diff @<context> [all]` | compare the selected object (or the whole namespace) with another cluster |
| `:export csv\|json\|md [file]` | save the visible table |
| `:audit` | log of every change kubesage made, by you or the AI |
| `:upgrade [1.36]` | **upgrade readiness**: deprecated/removed APIs, node version skew, available AKS versions |
| `:rbac` · `:whocan <verb> <resource>` · `:why` | your permissions here · who has access · why the last action was forbidden |
| `:pin` · `:pin ns` · `:alerts` | **watch list**: alert (bell + desktop notification) on new problems and expiring certificates |
| `:find <text\|image\|ip\|label=v> [--all]` | search every connected cluster |
| `:quota` · `:policyreports` · `:backups` | quotas and node pool capacity · Kyverno/Gatekeeper/PSA results · Velero |
| `:prom <PromQL>` · `:report` | query Prometheus · save a Markdown incident report |
| `:restart-needed` | workloads whose ConfigMap or Secret changed after their pods started; `R` restarts them |
| `:autoscaling` · `:mesh` · `:secrets-health` · `:opencost [workload]` | HPA/KEDA/VPA · Istio/Linkerd sidecars · secret sync and rotation · cost from OpenCost/Kubecost |
| `:kubeconfig` | contexts: check, rename, delete, **add AKS clusters** from `az` |
| `:runbooks` | team runbooks; the AI follows matching ones automatically |
| `:apply <path>` | server-side dry-run diff of local YAML, kustomize or a Helm chart, then `A` applies |
| `:fwd save <name>` · `:fwd <name>` | port-forward profiles |
| `( )` · `:cols` · `:mouse` | sort by column / reverse · show or hide columns · mouse support |
| `A` / `:ai <question>` | **AI chat** about the cluster |
| `?` | help |

### Multiple panes

The screen splits into as many as 6 panes, in the style of tmux or vim. Each pane has its own resource, namespace, filter, cursor, YAML/describe view or live logs,
and all of them refresh together. The focused pane has a thick accent border, and the header shows the panes, e.g. `▦ [1:Pods] 2:Events 3:AI`.

| Key / command | Action |
|---|---|
| `\|`  `:vsplit [resource]` | split side by side (e.g. `:vsplit events`, `:vsplit ai`, `:vsplit pulse`) |
| `-`  `:split [resource]` | split stacked |
| `L` | open the selected pod's (or workload's) **logs in a new pane** next to the table |
| `ctrl+w` / `alt+1`…`alt+6` | focus the next pane / pane N |
| `z`  `:zoom` | zoom the focused pane to full screen and back |
| `[`  `]` | shrink / grow the focused pane |
| `ctrl+x`  `:close` / `:only` | close this pane / keep only this pane |
| `n`  `:ns <ns>` / `:ns! <ns>` | namespace of the focused pane / of every pane |
| `:layout <name>` | presets: `triage` (pods \| events / AI), `ops` (deployments / pods \| events), `dash` (pulse \| events), `ai` (table \| AI), `single` |

There is one AI conversation, shown in whichever pane is in AI mode. Pressing `a` on a row in another pane sends the analysis to that AI pane, and the table stays where it is.

### Actions on the selected row

| Key | Action |
|---|---|
| `enter` / `d` | describe (`enter` on a namespace switches to it) |
| `y` | YAML (secrets are masked; `x` unmasks after confirmation) |
| `h` | **pod health report** (usage vs limits, probes, restarts, conditions, advice) |
| `l` / `p` | live logs / previous (crashed) container logs, with container and pod pickers |
| `s` | **shell into a container**: full TTY with resize, bash → ash → sh, no kubectl needed, debug-container fallback for distroless images |
| `a` | **AI analyze**: sends the manifest, events, detected issues and recent and previous logs to the model |
| `e` | **safe edit**: your editor → diff → server dry-run → apply on request |
| `o` | **rollout history** of a deployment: diff revisions, `U` to roll back |
| `w` | **relationships**: Ingress → Service → Deployment → ReplicaSet → Pods, plus ConfigMaps, Secrets, PVCs and HPA, coloured by health |
| `t` | **timeline**: events, rollouts, container restarts, scaling and pod starts of the object and its parts, newest first |
| `P` | **network policies** of a pod: who may connect, where it may connect, missing DNS egress |
| `N` | **network debug** from inside a pod: DNS, TCP and HTTP to a service, host:port or URL (ephemeral debug container, asks first) |
| `p` on a serviceaccount | its RoleBindings, ClusterRoleBindings and rules |
| `W` | **startup breakdown** of a pod: scheduling, image pulls, init containers, readiness, with probe advice |
| `B` | **browse files** in a container, view them, `:download` / `:upload` (no shell needed beyond `ls`/`cat`) |
| `u` on a Helm release | **upgrade preview**: `helm upgrade --dry-run=server` diffed against what runs now |
| `h` / `J` / `u` on a cronjob | run history with durations and failures / run now / suspend or resume |
| `S` / `R` / `D` | scale, rollout restart, delete |
| `f` | list port-forwards: open, go to, stop |
| `F` | **port-forward** a pod, service, deployment, statefulset or daemonset (services resolve named target ports to a ready pod) |
| `!` | type and run any `kubectl` command on the current context; the output opens in the viewer |
| `/` | filter, `n` namespace picker, `c` context picker, `T` cycle theme |

### Pod health

- With metrics-server installed, the **pods table shows live `CPU` and `MEM`**, plus the % of the limit when one is set (e.g. `822Mi 55%`).
  Pods at ≥ 90% of a limit turn yellow. The columns are left out when metrics are unavailable or not allowed.
- **`h`** on a pod opens its **health report**, which refreshes every few seconds. On a deployment, statefulset, daemonset or job, `h` first lets you pick a pod. The report shows:
  - a verdict: ✓ Healthy, ▲ Degraded or ✗ Unhealthy, with the reason;
  - status, phase, QoS class, node, IP, age, and every pod condition with how long it has held;
  - per container: state, readiness, restarts and the last exit reason (e.g. `OOMKilled (exit 137) 2h ago`), and the image;
  - CPU and memory bars: live usage against the limit, or against the request when there is no limit, with requests and limits;
  - liveness, readiness and startup probes (type, port, path, timings);
  - advice: no memory limit, no requests (BestEffort), missing probes, close to OOM, CPU throttling;
  - detected issues with hints, and the latest warning events.
  From the report, `a` asks the AI about this pod and `l` opens its logs.

### Secrets

`enter` (or `x`) on a Secret opens the **secret viewer**:
- **Masked by default.** You see each key's name, size and type (text, JSON, PEM certificate or key, binary). For certificates it also shows the subject, DNS names and **expiry**, which is flagged when it is under 30 days away or already past.
- **`x` unmasks** after a confirmation. On production-like contexts you have to type the secret's name. Values are decoded from base64 locally, JSON is pretty-printed and binary values are shown as hex.
- Values **mask again automatically after 60 seconds**, when you press `x`, or when you leave the viewer, and the decoded copy is dropped from memory.
- Decoded values are never written to disk and never sent to the AI. AI analysis always uses the redacted manifest.
- `y` (YAML) uses the same `x` unmask with confirmation.

In the Pulse screen, `enter` analyzes the selected issue and `t` asks the AI to triage every issue at once.

### Shell into a pod

Press **`s`** on a pod, or on a deployment, statefulset or daemonset (you pick the pod), or inside a pod's logs or health view.
With more than one container you pick one; init containers are left out because they have already exited.

- The app hands the terminal over to a full interactive shell, with colours, tab completion and window resizing, and the IDE comes back when you `exit` or press Ctrl-D.
  A banner shows the pod, the container and the cluster you are in.
- It picks the best shell in the image: `bash`, then `ash`, then `sh`.
- kubesage connects to the API itself, so **kubectl is not needed**, and it works the same in bash, zsh, PowerShell and Windows Terminal.
  It uses the WebSocket protocol (Kubernetes 1.30+) and falls back to SPDY. Some RBAC setups allow `pods/exec` only over WebSocket (GET).
- On production-like contexts it asks before opening the shell.
- **Images without a shell** (distroless or scratch): kubesage offers to attach an ephemeral `busybox` debug container that shares the container's processes
  (`kubectl debug --target`). The image is `busybox:1.36` by default; set `debug_image` in the config (or `:debug` on a pod) to use one from your own registry, e.g. `myacr.azurecr.io/tools/netshoot:1.0`. It asks first, because this permanently adds an ephemeral container to the pod, and it is unavailable in `--readonly` mode.
- For a single command without an interactive shell, use `!exec <pod> -- <cmd>`. It goes through the same approval as other changes.

### Logs

- **Search:** `/` searches any log, YAML or describe view (case-insensitive) and highlights every match. `n` / `N` jump to the next or previous match, keeping wrapped lines in view, and `esc` clears the search.
- **Time window:** `t` picks *last 500 lines, 5m, 15m, 1h, 6h or 24h*, and `T` toggles timestamps. Both restart the stream.
- **All pods at once:** on a deployment, statefulset, daemonset or job, `l` offers **★ all N pods, merged**. It follows one container in up to 20 pods in a single stream,
  with each line prefixed by a colour-coded `[pod]` tag (the pod name's suffix). Search works across the merged stream.

### Resource trends

kubesage keeps about 15 minutes of CPU and memory samples per pod in memory (one every 10 s, from the pods table and health reports).
The pods table gains a **`MEM 15m`** sparkline column once it has data and there is room. The **health report** (`h`) shows CPU and memory trend lines with start → now, minimum and maximum, so a pod creeping toward its memory limit stands out.

### Rollout history and rollback

`o` on a deployment opens its **revision history**: every ReplicaSet revision with its images, ready replicas, age and change-cause, plus the live rollout status
(`✓ rolled out`, `rolling out: 2 of 3 new replicas updated` or `✗ rollout stuck`).
- `enter` / `d` diffs a revision's pod template against the current one (the ReplicaSet hash label is ignored).
- `U` rolls back to that revision, like `kubectl rollout undo --to-revision`. It asks first, needs the name typed on production-like contexts, and is blocked in read-only mode.
- `esc` returns to the deployments list.

### Safe edit

`e` opens any object in your editor (`$KUBE_EDITOR`, `$EDITOR`, `$VISUAL`, or `vi` / `notepad`), with `managedFields` and `status` stripped. When you save and close:
1. kubesage shows a coloured **diff** against the live object.
2. It runs a **server-side dry-run**: the API server validates the change, including admission webhooks and policies, **without persisting it**.
   Rejections are shown with the reason, and policy **warnings** are listed (they are captured instead of being printed over the screen).
3. `A` applies the change after confirmation, `e` reopens the editor with your changes, and `esc` discards them.
If someone changed the object in the meantime, apply fails safely and your edits are kept. Renames and namespace moves are refused, and `--readonly` blocks editing. kubectl is not needed.

### Policy & security checks

`V` (or `:policy`) checks the pod template of every Deployment, StatefulSet, DaemonSet and CronJob, plus bare pods, in the focused namespace (or all namespaces):

| Severity | Checks |
|---|---|
| ✗ error | privileged containers · host network/PID/IPC (a warning for DaemonSets, where node agents need it) · images outside `allowed_registries` |
| ▲ warn | may run as root · privilege escalation allowed · writable root filesystem · capabilities not dropped · missing CPU/memory limits · no requests · `:latest`/untagged images · hostPath volumes |
| info | service-account token mounted · missing readiness/liveness probes |

`enter` opens the workload YAML, `e` safe-edits it, and **`a` asks the AI for the smallest patch** that fixes all findings of that workload. The AI is told not to apply anything and to explain findings that should stay.
Filter with `/` (e.g. `root`, `limits`). From the command line, `kubesage scan --policy` exits with code 2 when there are errors, which suits CI.
```yaml
policy:
  allowed_registries: ['^[a-z0-9-]+\.azurecr\.io/']   # optional; empty = registry check off
  skip_namespaces: [kube-system, kube-public, kube-node-lease, gatekeeper-system, calico-system, tigera-operator, azure-workload-identity-system]
```

**Your own rules (policy as code).** Add checks to the config; they show up in `V` / `:policy` and in `kubesage scan --policy` (so CI fails on them too):

```yaml
rules:
  - name: owner label
    require_labels: [owner, app.kubernetes.io/name]       # kinds default to deployments, statefulsets, daemonsets
    severity: error
  - name: memory limit
    path: spec.template.spec.containers[*].resources.limits.memory   # [*] = every item; [0] = the first
    message: every container needs a memory limit
  - name: no latest tag
    path: spec.template.spec.containers[*].image
    forbid: ':latest$'
  - name: ingress class
    kinds: [ingresses]
    namespaces: '^team-'                                 # regex; default = all except skip_namespaces
    path: spec.ingressClassName
    match: '^(nginx|internal)$'
```
A rule may require labels/annotations, require a path to exist (`path` alone), require every value to `match` a regex, or `forbid` values.

### Troubleshooting views

**Relationships (`w`)** on an ingress, service, deployment, statefulset, daemonset, replicaset or pod draws the tree around it,
with what sits in front of it (Services, Ingresses) on top. A Service that selects no pods, an Ingress pointing at a missing Service,
an unbound PVC or a crashing pod shows in red, so "why does this ingress return 503?" is one screen. Objects managed by Argo CD,
Flux or Helm are marked.

**Timeline (`t`)** answers "what changed just before it broke?": warning and normal events, new ReplicaSets (with their images),
container terminations with exit codes, HPA scaling and pod starts, merged and grouped by day. `r` switches back to the relationships.

**AI incident mode (`I`)** collects detected issues, warning events of the last hour, failed Helm releases, unbound PVCs, node problems and
rollouts of the last two hours, then asks the AI to group symptoms into incidents, name the root cause, gather missing evidence with
read-only commands and propose fixes. Every change still needs your approval (and is blocked in `--readonly`).

**Right-sizing (`:rightsize`)** compares each workload's requests with its peak usage (metrics-server now, plus what kubesage has seen in the
last 15 minutes) and suggests CPU = peak × 1.5 and memory = peak × 1.3. It flags over- and under-provisioned workloads and missing requests, totals the
reclaimable CPU and memory and, with `cost` in the config, the monthly waste. `enter` shows the `kubectl set resources` command, `e` safe-edits the workload.

**Cluster diff (`:diff @<context>`)** compares the selected object with the same one on another cluster, ignoring status, ids, timestamps and generated fields;
Secret values are compared by hash, never shown. `:diff @<context> all` lists every object of the current type in the namespace as differs / only here / only there / same.

### Operations

**Upgrade readiness (`:upgrade [version]`)** checks the next (or given) Kubernetes version against: the manifests of every Helm release
(what the next `helm upgrade` sends), `kubectl apply` last-applied annotations, and the API server's own count of deprecated API calls
(`apiserver_requested_deprecated_apis`, when `/metrics` is readable). Uses of APIs removed by the target are red. It also shows kubelet versions per
node pool and, on AKS with the `az` CLI signed in, the versions the cluster can upgrade to.

**RBAC explorer.** `:rbac` draws a grid of what you may do in the namespace (asked of the API server as you). `:whocan get secrets` lists the users,
groups and service accounts that have that permission and through which binding. After a "forbidden" error, `:why` explains it and who holds the permission.
`p` on a service account shows its bindings and rules.

**Watch list and alerts.** `:pin` watches the selected workload, `:pin ns` the whole namespace, on any cluster. Every 30 s kubesage checks them in the background
(also for TLS certificates expiring within 14 days) and announces new problems with the terminal bell, a desktop notification (macOS, Linux) and a 🔔 counter in the header.
`:alerts` lists active and resolved problems; `enter` opens the object, `a` asks the AI. Problems that already exist when kubesage starts do not ring.

**Prometheus.** With a Prometheus-compatible endpoint, right-sizing uses peaks over 7 days instead of 15 minutes, and `:prom <PromQL>` shows the top series with sparklines.
kubesage looks for common in-cluster services (`prometheus-operated`, `prometheus-server`, … in `monitoring`) and reaches them through the API server proxy, or set:

```yaml
prometheus:
  urls:
    "*": http://localhost:9090                         # or service:monitoring/prometheus-operated:9090
  bearer_token_command: az account get-access-token --resource https://prometheus.monitor.azure.com --query accessToken -o tsv
  window: 7d
```

**Network debug (`N` on a pod)** runs DNS, TCP and HTTP checks from inside the pod's network namespace with an ephemeral `debug_image` container. Pair it with `P` (network policies).

**Also:** `:find` searches names, images, IPs, hosts and labels across every connected cluster (`--all` for every kubeconfig context); `:quota` shows ResourceQuota usage,
LimitRanges and request-based node pool utilisation with nodes that could be removed; `:policyreports` collects Kyverno PolicyReports, Gatekeeper violations
and namespace Pod Security levels; `:backups` lists Velero schedules and backups; `:report` writes relationships, timeline, open issues and the last AI answer to Markdown.

### Day-2 helpers

**Restart needed (`:restart-needed`).** Environment variables and `subPath` mounts never update in a running pod. kubesage compares when each ConfigMap and Secret
a pod uses last changed (from managed fields) with when the pod started, and lists the workloads that still run the old values. `R` does a rollout restart.
Plain volume mounts are listed as "updated in place" because the file changes but the app may not reload it.

**Pod startup (`W`)** breaks a pod's start into scheduling, image pulls (from events), each init container, container start and readiness, and flags
risky probes: liveness without a startup probe on a slow app, 1-second liveness timeouts, liveness and readiness on the same endpoint, missing readiness.

**Autoscaling (`:autoscaling`)** shows HPAs (current vs target per metric, stuck at max, metrics unavailable), KEDA ScaledObjects (triggers, ready, paused) and
VPAs (recommendations). VPA recommendations also appear as a column in `:rightsize`.

**Kubeconfig (`:kubeconfig`)** lists contexts with server and auth plugin. `p` checks one, `r` renames, `D` deletes (with unused cluster and user entries), and `a`
adds an AKS cluster: pick a subscription and a cluster from `az`, and kubesage runs `az aks get-credentials`. Every change writes a backup next to the kubeconfig first.

**Runbooks.** Point `runbooks: {dir: ~/runbooks}` at a folder (or a git clone; `:runbooks pull` updates it). Each Markdown file can declare what it is for:

```markdown
---
title: Pod crash loops
match: [CrashLoopBackOff, OOMKilled]
---
# Crash loops
1. Check the previous container's logs (p) ...
```

Whenever the AI analyses, triages or answers an incident, matching runbooks are added to the prompt and the AI is told to follow them first. In `:alerts`, `r` opens the matching runbook.

**Logs:** `J` renders JSON lines as `LEVEL time message key=value`, `i`/`x` keep or hide lines by regex, `ctrl+s` saves what is shown.

**Apply local manifests (`:apply ./deploy`)** renders a file, a folder of YAML, a kustomization (`kubectl kustomize`) or a Helm chart (`helm template`), runs a
server-side dry-run apply of every object and shows new / changed (with a diff) / unchanged / errors. `A` applies with server-side apply; nothing is applied while any object fails.

**Secrets health (`:secrets-health`)** lists Secrets with the time since their last change, how many pods use them, and whether they are managed by an ExternalSecret
or SealedSecret (with sync status). Secrets older than `secret_rotation_days` (90) are yellow. Values are never read into this view.

**Gateway API and mesh:** the relationships graph follows HTTPRoutes and Gateways (`w` works on them too) and marks Istio/Linkerd sidecars that are not ready;
`:mesh` shows injection per namespace and pods that are missing their sidecar. **Cost:** `:opencost` reads OpenCost or Kubecost (auto-detected, or `opencost.url`).

### Safety for production

```yaml
change_windows:                 # changes to matching contexts only inside the window
  - {contexts: "(?i)prod", days: Mon-Fri, hours: "08:00-18:00", tz: Europe/Amsterdam, mode: block}   # or mode: warn
freezes:
  - {contexts: "(?i)prod", from: 2026-12-20, to: 2027-01-03, reason: holiday freeze}
require_reason: "(?i)prod"      # every change asks for a reason, stored in the audit log
```

Blocked changes include the AI's commands. **Port-forward profiles:** start the forwards you need, `:fwd save dev-stack`, and next time `:fwd dev-stack` starts them all.

**Table comfort:** `(` sorts by the next column and `)` reverses (ages, sizes and counts sort numerically). `:cols` hides columns per resource (saved as `hide_columns`).
`:mouse` turns on clicking rows and headers and wheel scrolling (hold Option/Shift to select text). `keys: {w: g}` remaps a key.

### Insights

| Command | Shows |
|---------|-------|
| `:certs` | every TLS Secret and cert-manager Certificate: subject, DNS names, issuer, expiry, days left (yellow under 30, red when expired or not Ready). Only the public certificate is read |
| `:images` | every image in use: tag, registry, `latest`/untagged warnings, digest pinning, the digest actually running, pods and workloads. `enter` runs a **Trivy** scan for HIGH/CRITICAL vulnerabilities when `trivy` is installed |
| `:storage` | PVCs by fill level (red at 90 %), capacity, storage class, access mode and the pods mounting them; usage comes from the kubelet summary API and needs `nodes/proxy` |
| `:gitops` | Argo CD Applications and Flux Kustomizations/HelmReleases with sync, health, revision and source. Safe edit warns when the object is managed by GitOps, because the next sync reverts a manual change |

### Command palette, export, plugins and audit log

- **`ctrl+p`** opens a fuzzy palette over kubesage's screens, every resource type, saved views, plugins and kubeconfig contexts.
- **`:export csv|json|md [file]`** writes the visible (filtered) rows of any table, handy for incident reports.
- **Custom columns** add fields to any table:

  ```yaml
  columns:
    pods:
      - {name: image, path: spec.containers[0].image}
      - {name: team, label: team}
    deployments:
      - {name: owner, annotation: example.com/owner}
  ```

- **Plugins** bind your own commands to a key on selected resources. Variables: `$NAME $NAMESPACE $CONTEXT $KIND $RESOURCE $KUBECONFIG`.
  They ask before running unless `confirm: false`, and `background: true` shows the output in the viewer instead of handing over the terminal.

  ```yaml
  plugins:
    - name: stern
      key: K
      scopes: [pods, deployments]
      command: stern
      args: [$NAME, -n, $NAMESPACE, --context, $CONTEXT]
  ```

- **Audit log**: every change (scale, restart, delete, edit, rollback, drain, cronjob runs, plugins, commands the AI ran after approval) is appended to
  `audit.log` next to `config.yaml`, with time, context, target, who (you or the AI) and the result. `:audit` shows it; `audit_log: false` turns it off.

### Helm releases

`:helm` lists the Helm releases in the namespace (or `:ns all` for every namespace), read directly from Helm's release Secrets and ConfigMaps, so no `helm` CLI is needed to browse.

| Key | Does |
|-----|------|
| `enter` / `v` | user-supplied values; keys that look secret (password, token, key…) are masked, `x` unmasks after confirmation and masks again after 60 s |
| `m` | rendered manifest |
| `h` | revision history; `enter`/`d` diffs a revision's values and manifest against the current one, `U` runs `helm rollback` after a typed confirmation (needs the `helm` CLI, blocked in `--readonly`) |

`:charts` (or `:helm charts`, or `M` twice) groups the releases by chart: versions and app versions in use, how many releases, in which namespaces, and whether any failed.
The chart files come from the chart packaged with the newest release, so no repository access is needed.

| Key | Does |
|-----|------|
| `enter` | the releases that use this chart |
| `v` | the chart's default values (secret-looking keys masked, `x` to unmask) |
| `r` | README |
| `t` | all templates, one after another (`/` to search) |
| `i` | metadata: home, sources, keywords, dependencies, packaged files |

Both screens are also at the top of the resource list (`0`), in a **Helm** section.

### Nodes

On `:nodes`:

| Key | Does |
|-----|------|
| `h` | **node health**: conditions, taints, allocatable vs requests/limits vs live usage, and every pod on the node; refreshes live |
| `u` | cordon / uncordon |
| `X` | **drain preview**: which pods would be evicted, which are skipped (DaemonSet, mirror pods) and which would block the drain (no controller, a PodDisruptionBudget allowing 0 disruptions); `A` in the preview drains after you type the node name |
| `:taint key=value:NoSchedule` / `:taint key-` | add or remove a taint on the selected node |

All of these are blocked in `--readonly` mode and ask for confirmation.

### Saved views

A view remembers a tab's cluster, namespace, resource and pane layout.
`:view save triage-shop` stores the current tab in the config, `b` (or `:view`) lists views and opens the chosen one in a new tab (connecting to its cluster when needed), `:view rm <name>` deletes one.

### Live updates

Tables follow the cluster with Kubernetes **watches**, so a pod that crashes or a deployment that scales shows up within about a second; the title shows `● live`.
When a watch is not allowed (for example `:pods all` with namespace-scoped RBAC) the pane falls back to polling every `refresh_seconds` and shows `↻ 3s`, and retries the watch every 30 s.
`:live poll` / `:live watch` switches the mode and saves it as `live_updates` in the config.

### Port-forwarding

Press `F` on a row and enter the ports as `local:remote`. It is pre-filled; ports below 1024 map to 8000+port locally, and `:80` picks a random local port.
Once the tunnel is up, kubesage **opens `http://localhost:<port>` in your default browser** (`https` for remote port 443/8443).
This works on macOS, Linux (`xdg-open`, or `wslview` under WSL) and Windows. Set `open_browser: false` to turn it off.

Rows that are being forwarded are marked in the tables:

| Mark | Meaning |
|---|---|
| `⇄` + `NAME  ⇄ :8080` | this pod, service or workload is being forwarded to local port 8080 |
| `↔` + `NAME  ⇄ via svc/web :8080` | this pod receives the traffic of a forward started on a service or workload |

Pressing `F` on a row that is already forwarded doesn't start a duplicate. You get a menu instead: open it in the browser again, stop it, or start another one.
Forwards keep running in the background while you browse, even if you switch context.

**Finding a forward you forgot about.** The header always says what is forwarded and where, for example
`⇄ svc/grafana :8080 in nsco-monitoring  f`. With several forwards it shows `⇄ 3 forwards · 2 elsewhere  f`. Press **`f`** (or `:pf`) for the list.
Each entry shows the object, its URL, its context and namespace if they differ from your view, and how long it has been up. Pick one to:
- **Open** it in the browser again
- **Go to** it: kubesage switches resource, namespace and context as needed and puts the cursor on the forwarded row
- **Stop** it (or *Stop all* from the list)

The Pulse screen (`H`) lists active forwards as well.
All forwards stop when you quit. If a forward dies, for example because its pod restarted, the status line tells you.

## AI that runs commands, with your approval

When `allow_commands` is on, the AI can investigate on its own and propose fixes:

1. It writes the commands it wants to run in a ```` ```run ```` block. kubesage parses them and checks each one.
2. **Read-only** commands (`get`, `describe`, `logs`, `top`, `events`, `rollout status/history`, `auth can-i`, any `--dry-run`)
   run immediately. Their output goes back to the model, and it carries on until it reaches a conclusion. Each question gets at most `max_steps` rounds.
3. **Changing** commands (`apply`, `patch`, `scale`, `rollout restart/undo`, `delete`, `set`, `label`, `cordon`, `drain`, `exec`, …)
   open an approval dialog. `y` runs it, `n` skips it, `e` lets you edit it first, `a` approves the rest of this step, and `esc` stops the batch.
   On production-like contexts you have to type `approve`, and approve-all is disabled.
4. **Blocked**, never executed: pipes and other shell syntax, `--watch`, `logs -f`, `exec -it`, `port-forward`, `edit`, `proxy`, `cp`.
5. The AI can never pick a different cluster or identity. `--context`, `--kubeconfig`, `--as`, `--token` and similar flags are stripped,
   and every command runs against the context you are viewing. Reading secret values (`get secret -o yaml`, `--raw`) always needs approval.
6. In `--readonly` mode, changing commands are refused without asking, and the AI is told they were not run.

In the AI chat, `ctrl+r` lets you pick any `kubectl` command from the last reply (including ```` ```bash ```` suggestions) and run it through the same checks.
`kubesage ask` uses the same loop in a plain terminal and asks `[y/N]` for each change.

> Small local models (e.g. codellama) often ignore the `run` protocol and only suggest commands. Instruction-following models
> such as Claude, GPT-4o, `llama3.1`, or `qwen2.5` work well.

## Safety

- If a context name matches `danger_contexts` (default `(?i)(prod|prd|-p$|live)`), the header turns red
  and you have to type the object's name before a delete goes through.
- `--readonly` or `read_only: true` blocks every mutating action, including ones the AI proposes.
- With `redact_secrets: true` (the default), nothing sensitive is sent to an LLM: Secret values and env vars whose
  names look sensitive (`*PASSWORD*`, `*TOKEN*`, `*KEY*` and similar) are replaced with `<redacted>` first.

- `ai.local_only_contexts` (a regex) keeps the data of matching clusters on your computer: there, the AI only works with
  a local model (Ollama, or an OpenAI-compatible server on localhost).

## AI fixes, narratives and plain-English search

**AI fix with preview (`:fix`, or space → AI fix).** The AI writes the *smallest* patch for the selected object, never a whole manifest.
kubesage merges it into the live object on your computer (strategic merge, so containers match by name), then shows exactly what a
safe edit shows: the server-side dry-run verdict (validation, admission webhooks, policies), warnings and the diff, with the AI's reasons
on top. Nothing changes until you press `A` and confirm; the change is audited as `ai-fix.apply`. Patches that touch a `<redacted>` value
are refused, so masked secrets can never overwrite real ones, and Secrets are never sent.

**Incident narrative (`:narrative`).** Turns the timeline of the selected object (events, rollouts, restarts, scaling, and the locally
stored history) into a summary, a chronological account, the likely cause and follow-ups, ready for a postmortem (`:report` saves Markdown).

**Plain-English search (`:nl`).** `:nl pods restarting more than 3 times in shop` becomes the right resource, namespace and filters
(including a restart threshold); the status bar says what is shown and `esc` clears it.

**Cost hint.** The AI pane title shows an estimate of the tokens used in this session and, when the profile has prices, the cost:
```yaml
ai:
  local_only_contexts: '(prod|-p$)'      # cloud AI refused here; use a local model
  profiles:
    claude: {provider: anthropic, model: claude-opus-5-5, api_key_env: ANTHROPIC_API_KEY, price_in_per_mtok: 5, price_out_per_mtok: 25}
```

## Teams: approvals, shared settings and a tamper-evident audit log

**Two-person rule.** Changes on matching contexts need a teammate's approval. Instead of applying, kubesage stores a request
(a ConfigMap in `kubesage-approvals`, which you create once and restrict with RBAC) and, if `reports.webhook_env` is set, posts it to
Slack/Teams. A teammate approves in `:approvals` (`y` approve, `n` reject) or with `kubesage approve <id>`; the requester can never
approve their own request. Your kubesage notices the approval within 15 seconds, and `enter` on it runs the change after one last
confirmation. Requests expire after an hour and can run once.
```yaml
approvals: {contexts: '(prod|-p$)', namespace: kubesage-approvals, expire_minutes: 60}
```

**Shared team settings.** Point kubesage at a git repository with a `kubesage.yaml`; at startup it is cloned or updated and its policy
rules, schedules, fleet list, change windows, freezes, allowed registries, danger contexts, approval rule and runbooks are added to
yours (your own scalar settings win, and nothing from the team file is written into your config). Plugins in the team file run
commands on your computer, so they are only loaded with `allow_plugins: true`. `:team` shows what was loaded.
```yaml
team: {repo: https://github.com/acme/platform-kubesage.git, ref: main, path: kubesage.yaml}
```

**Tamper-evident audit log.** Every entry carries a SHA-256 hash of itself and the previous entry. `kubesage audit verify` reports a
changed or deleted line; `kubesage audit export --since 24h` pushes entries to Grafana Loki (`audit_loki_url`, token in `$KUBESAGE_LOKI_TOKEN`).

## Logs, traces, node pools and operators

| Command | What it does |
|---|---|
| `:loki [6h] [text or LogQL]` | search logs kept by Loki, also of deleted pods; without LogQL it searches the selected pod/workload. Loki is found in the `loki`, `monitoring`, `logging` or `observability` namespace, or set `loki: {url: …}` |
| `:trace` | in logs or Loki results: pick a trace ID (`trace_id=`, `traceparent`, `x-b3-traceid`) and open it in your tracing UI (`traces: {url: "https://grafana/…{trace_id}"}`) |
| `:nodepools` | AKS agent pools, EKS node groups, GKE node pools and Karpenter NodePools from node labels: nodes ready, spot, instance types, zones, kubelet versions, recent spot evictions. `S` scales an AKS pool (az) or EKS node group (aws) after confirmation |
| `:resize cpu=250m memory=512Mi` | on a pod: change CPU/memory **in place**, without a restart (Kubernetes 1.33+). Dry-run first, then confirm; the workload template is not changed |
| `:argo-rollouts` | Argo Rollouts with strategy, step and phase: `p` promote, `P` promote fully, `X` abort, `T` retry |
| `:karpenter` | NodePools with limits, usage, disruption policy and NodeClaims; `e` safe-edits a NodePool |
| `:crossplane` | every Crossplane managed resource with Ready/Synced and the external name |
| `:flows` | network flows of the selected pod or namespace from Cilium Hubble, with drops highlighted (needs the `hubble` CLI and `cilium hubble port-forward`) |

`:secrets-health` now also shows where External Secrets come from (Azure Key Vault, Vault, AWS, GCP, … and the remote keys) and
lists Secrets Store CSI `SecretProviderClass`es.

## Developer workflow

**Dev loop (`:dev <path>`).** Watches a file or folder (plain YAML, kustomize or a Helm chart) and server-side-applies it to the current
namespace every time you save, reporting what changed. Refused on production-like contexts and "all namespaces". `:dev stop` ends it.

**Preview namespaces (`:clone <new-namespace> [24h]`).** Copies the Deployments, StatefulSets, CronJobs, Services, ConfigMaps and
Secrets of the current namespace into a new one labelled with its origin and an expiry time. Load balancers become ClusterIP services;
Ingresses and volumes are not copied. `:clones` lists them (expired ones are highlighted) and `D` deletes one; kubesage only deletes
namespaces it created. Refused on production-like contexts.

**Plugin catalogue (`:plugins`).** Lists plugins from [`plugins/index.yaml`](plugins/index.yaml) in this repository (or `plugin_index:` in
your config). `enter` shows the exact command it will run and installs it on confirmation; `D` removes it. Pull requests with new plugins are welcome.

**Start with a command (`--cmd`).** `kubesage --cmd ":apply ./k8s"` or `--cmd ":dev ./k8s"` opens straight into that command.

## In the browser and in VS Code

**Web UI (`kubesage serve`).** The full kubesage in a browser tab, for teammates who do not live in a terminal. Each tab is its own session.
It is **read-only unless you pass `--allow-changes`**, listens on 127.0.0.1:7777 unless you pass `--lan` (then anyone with the link can use
kubesage with *your* credentials), and every request needs the random token in the printed link. Shells, `$EDITOR` and plugins are disabled
in the browser because they would run on the server's terminal.
```bash
kubesage serve                        # http://127.0.0.1:7777/?t=…
kubesage serve --lan --addr :8080     # on your network, read-only
```

**VS Code.** [`editors/vscode`](editors/vscode) is a small extension: open kubesage in an editor tab (`Cmd/Ctrl+Alt+K`), pick a cluster,
open read-only, or right-click a YAML file (diff and apply) or folder (dev loop). Build it with `npx @vscode/vsce package`.

## Workspace tabs

Tabs sit above panes: each tab has its own split layout, so you can keep for example `shop` debugging in one tab,
`kube-system` in another and a monitoring dashboard in a third. Background tabs keep their log streams running.

| Key / command | Action |
|---|---|
| `ctrl+t`  `:tabnew [namespace\|resource]` | new tab (e.g. `:tabnew kube-system`, `:tabnew events`, `:tabnew all`) |
| `>` `<`  `:tabn` `:tabp`  `:tab N` | next / previous / go to tab N |
| `:tabname <name>` | rename the tab (default label: `namespace·view`) |
| `:tabclose`, or `ctrl+x` on a tab's last pane | close the tab |

The tab strip is shown at the start of the second header line once you have more than one tab.

## Multiple clusters

Every pane has its own cluster connection, so one session can work across several clusters. Connections are pooled:
each context signs in once and is shared by every tab and pane that uses it.

There are two ways to choose clusters:

- **On the command line:** `kubesage --context staging` or `kubesage --context staging,prod-eu,kind-dev` (each in its own tab).
- **In the app:** start plain `kubesage` and a **cluster chooser** lists every kubeconfig context. The cursor starts on your
  current context. `enter` opens it, `space` ticks several and `enter` opens them all (one tab each), `/` filters, `q` quits.
  No cluster is contacted until you choose, so nothing triggers a sign-in. If the connection fails, you stay in the chooser with the error.
  The chooser is skipped when the kubeconfig has only one context. To turn it off, set `choose_context_on_start: false`, which starts on the current context.

| Key / command | Action |
|---|---|
| `C`  `:clusters` | **Clusters overview**: every kubeconfig context with reachability, version, ready nodes, API latency, open panes and a `⚠ prod` mark. `enter` opens it in a new tab, `v` in a side-by-side pane, `c` switches this tab to it, `p` runs a health check |
| `:tabnew @<context> [ns]` | new tab on another cluster |
| `:vsplit @<context> [ns]` / `:split @<context>` | open another cluster next to the current one, e.g. to compare nodes or a deployment between staging and production |
| `c`  `:ctx <name>` | switch the current tab (all its panes) to another cluster; other tabs keep theirs |

Once more than one cluster is open, every pane title starts with `⎈ <context>`, and production-like ones with `⚠`. Tab labels name their cluster, or `⎈×2 clusters` for a mixed tab.
Each pane acts on its own cluster: typed confirmations follow that pane's cluster, and Pulse shows that cluster.
AI analysis and the commands the AI runs are pinned to the cluster of the row you analysed, and the approval dialog names that cluster.
Unconnected clusters are only contacted when you open them or press `p`, because connecting can trigger a sign-in (e.g. Azure `kubelogin`).

## MCP server for AI agents

`kubesage mcp` serves kubesage's read-only insights over the Model Context Protocol (stdio), so Claude Code and other agents can use them:
`incident_report`, `find_issues`, `relationships`, `timeline`, `pod_logs`, `list_resources`, `helm_releases`, `upgrade_readiness`, `right_sizing`, `my_access`, `search`, `list_contexts`.
No tool changes the cluster.

```bash
claude mcp add kubesage -- kubesage mcp --context my-staging
```

## Configure AI

Run `kubesage config init`. The file goes to `~/.config/kubesage/config.yaml` on macOS and Linux, and to `%AppData%\kubesage\config.yaml` on Windows.

```yaml
theme: tokyonight            # tokyonight, dracula, nord, catppuccin, gruvbox, solarized-light
refresh_seconds: 3
live_updates: watch         # watch (live, falls back to polling) or poll
debug_image: busybox:1.36   # ephemeral debug container image for shell-less pods
cost:                       # optional: prices for right-sizing waste
  cpu_core_hour: 0.031
  mem_gib_hour: 0.004
  currency: EUR
audit_log: true
update_check: true          # release builds look for a newer version at startup
watchlist: []               # filled by :pin
alerts: {bell: true, desktop: true, cert_days: 14, interval_sec: 30}
read_only: false
choose_context_on_start: true   # show the cluster chooser when --context is not given
animations: true                # animated logo in the terminal
open_browser: true         # open localhost:<port> after a port-forward starts
danger_contexts: (?i)(prod|prd|-p$|live)
favorites: [pods, deployments, services, events, nodes]
ai:
  active: local              # switch at runtime with ctrl+o or :profile <name>
  redact_secrets: true
  log_lines: 120
  allow_commands: true       # let the AI run kubectl (```run blocks)
  auto_run_read_only: true   # false = ask before every command, even reads
  max_steps: 6               # command rounds per question
  profiles:
    local:                   # Ollama; model auto-detected if omitted
      provider: ollama
      base_url: http://localhost:11434
      model: llama3.1
    claude:
      provider: anthropic
      model: claude-opus-5-5
      api_key_env: ANTHROPIC_API_KEY
    openai:
      provider: openai
      model: gpt-4o-mini
      api_key_env: OPENAI_API_KEY
    azure:
      provider: azure
      base_url: https://<resource>.openai.azure.com
      model: <deployment-name>
      api_version: "2024-10-21"
      api_key_env: AZURE_OPENAI_API_KEY
    gemini:
      provider: gemini
      model: gemini-2.0-flash
      api_key_env: GEMINI_API_KEY
    lmstudio:                # any OpenAI-compatible server (LM Studio, vLLM, Groq, OpenRouter…)
      provider: openai-compatible
      base_url: http://localhost:1234/v1
      model: qwen2.5-coder
```

### Adding API keys

**Quickest: let kubesage store the key in your OS keychain** (macOS Keychain, Windows Credential Manager or Linux Secret Service).
The key is never written to the config file. If no keychain is available, for example on a headless Linux server, kubesage falls back to the config file (`0600`) and says so.

From the command line:
```bash
kubesage ai add claude --provider anthropic --model claude-opus-5-5 --use   # prompts for the key (hidden)
kubesage ai set-key openai                     # add or replace a key for an existing profile (hidden prompt)
op read op://Private/OpenAI/key | kubesage ai set-key openai   # or pipe it in, e.g. from a password manager
kubesage ai add groq --provider openai-compatible --base-url https://api.groq.com/openai/v1 --model llama-3.3-70b-versatile
kubesage ai add azure-gpt --provider azure --base-url https://myres.openai.azure.com --model gpt4o-deploy
kubesage ai use claude          # default profile
kubesage ai                     # list profiles and where each key comes from (never prints keys)
kubesage ai test claude         # one-line test call
kubesage ai delete-key openai   # forget a stored key
kubesage ai remove groq         # delete a profile and its key
```

Inside the app:
- **`:ai-add`** is a wizard: pick the provider, then enter a name, the model (pre-filled with a default) and, for Azure or OpenAI-compatible servers, the endpoint. It then asks for the key if none is found.
- **`:ai-key [profile]`** sets or replaces a key. Typing is masked (`•••`), the value is cleared from memory right after saving, and it is never shown on screen or sent to the AI.
- **`ctrl+o`** (or `:profile`) lists the profiles with their key status, plus *Add AI profile…* and *Set API key…*. Switching to a profile that has no key asks for one.
- **`:ai-rm <profile>`** removes a profile and its stored key.

Each profile finds its key in this order: the **environment variable** named in `api_key_env`, then **`api_key_cmd`**, then the **OS keychain**, then a literal **`api_key`**.
An environment variable that is set therefore overrides a stored key. `kubesage ai` shows which source is used.

```bash
# 1. Environment variable (add to ~/.zshrc or ~/.bashrc to keep it)
export ANTHROPIC_API_KEY=sk-ant-...
export OPENAI_API_KEY=sk-...
export GEMINI_API_KEY=...
export AZURE_OPENAI_API_KEY=...
```
```powershell
# PowerShell: current session, then permanently for your user
$env:ANTHROPIC_API_KEY = "sk-ant-..."
[Environment]::SetEnvironmentVariable("ANTHROPIC_API_KEY", "sk-ant-...", "User")
```
```yaml
# 2. A command that prints the key: macOS Keychain, 1Password, pass, Azure Key Vault…
    claude:
      provider: anthropic
      model: claude-opus-5-5
      api_key_cmd: security find-generic-password -s kubesage-anthropic -w
      # api_key_cmd: op read op://Private/Anthropic/credential
      # api_key_cmd: az keyvault secret show --vault-name my-kv -n openai-key --query value -o tsv
```
(Store a key in the macOS Keychain once with `security add-generic-password -s kubesage-anthropic -a $USER -w`; it prompts for the value.)

To add another provider or model, add a profile. You can have any number, including several per provider:
```yaml
    claude-fast:   {provider: anthropic, model: claude-haiku-5-5, api_key_env: ANTHROPIC_API_KEY}
    groq:          {provider: openai-compatible, base_url: https://api.groq.com/openai/v1, model: llama-3.3-70b-versatile, api_key_env: GROQ_API_KEY}
    openrouter:    {provider: openai-compatible, base_url: https://openrouter.ai/api/v1, model: anthropic/claude-sonnet-5-5, api_key_env: OPENROUTER_API_KEY}
    qwen-local:    {provider: ollama, model: qwen2.5:14b}
```

Check them and switch:
```bash
kubesage ai              # every profile with provider, model and whether its key was found (never prints keys)
kubesage ai test claude  # sends a one-line prompt to confirm the key and model work
kubesage --ai claude     # start with that profile
```
In the app, **`ctrl+o`** in the AI chat (or `:profile`) lists the profiles with the same key status. `★` marks the active one. `:save` makes your choice the default. The Anthropic profile uses the official Go SDK
and turns on server-side refusal fallback, so if a request is declined it is retried on a fallback model.

## Layout

```
main.go                 CLI entry (tui, scan, ask, config)
internal/config         YAML config + AI profiles
internal/kube           client-go wrapper: resource tables, actions, logs, rule engine, AI context
internal/agent          AI command execution: extraction, risk classification, runner
internal/ai             providers: ollama, anthropic (SDK), openai/azure/compatible, gemini
internal/ui             Bubble Tea app: split panes, themes, table, text/log viewer, AI chat, pulse, modals
```

## Logo

| File | Use |
|---|---|
| `assets/logo-animated.svg` | wordmark with a slowly turning wheel, a breathing leaf and twinkling sparkles (shown at the top of this README) |
| `assets/icon-animated.svg` | animated square icon |
| `assets/logo.svg`, `assets/icon.svg` | static versions for slides, favicons and places that do not play animation |

The animation is plain CSS inside the SVG, with no scripts. It plays wherever SVG is shown, including GitHub READMEs and browsers, and it stops for people who have
turned on "reduce motion" in their OS.
The helm wheel stands for Kubernetes, the sage leaf in its hub for the copilot, and the sparkle for AI. Inside the terminal the same mark appears in the header,
the cluster chooser, the help screen, the AI chat and `kubesage version`. It is animated there too: a light sweeps around the wheel,
the leaf breathes and the sparkle twinkles. The animation only changes colours, so it never shifts the layout. Set `animations: false` in the config for a static logo,
and `NO_COLOR=1 kubesage version` prints it without animation.

## Fleet, event history, schedules, chaos tests and sharing

**Fleet (`:fleet`).** One table with the health of many clusters: version, API latency, nodes ready, pods running, detected issues and
warning events, problems first. `enter` opens a cluster in a new tab. By default it shows the clusters you are connected to; list the
ones you care about (regular expressions) to always include them:
```yaml
fleet: ['^aks-.*-dev$', 'staging']
```

**Event history (`:history`).** Kubernetes forgets events after about an hour. While kubesage runs it saves the events of every cluster
shown in a pane once a minute (in `events/` next to the config file, kept 7 days). `:history` lists them, and the timeline (`t`)
merges them in, marked `(history)`, so you can see what happened yesterday. Turn it off or change the retention:
```yaml
event_history: {off: false, days: 14}
```

**Scale-to-zero schedules (`:schedules`).** Save money on dev and test clusters by scaling Deployments and StatefulSets to zero outside
office hours. kubesage remembers the replica count in the `kubesage.io/scaled-down-replicas` annotation and restores it in the morning.
Contexts matching `danger_contexts` are refused unless the schedule says `allow_danger: true`.
```yaml
schedules:
  - name: nights and weekends
    context: '-dev$'           # regex of kube-contexts
    namespace: shop
    selector: 'tier!=db'       # optional label selector
    down: "19:00"
    up: "07:00"
    days: [mon, tue, wed, thu, fri]   # other days: down all day
```
Run `kubesage schedule run` from cron (every 10 minutes is plenty) and `kubesage schedule status` (or `run --dry-run`) to see what it would do.
In the UI, `:schedules` shows whether each schedule is in step; `d` scales down now and `u` restores now (both ask first and go to the audit log).
If a HorizontalPodAutoscaler manages the workload, its `minReplicas` will scale it back up; use KEDA or pause the HPA for those.

**Chaos-light tests (`:chaos`, or space → chaos test).** On a Deployment, StatefulSet, ReplicaSet or pod: deletes **one** ready pod and
measures how long the workload takes to be fully ready again (up to 5 minutes), with advice when recovery is slow. You type the name to
confirm. It is refused on production-like contexts, in read-only mode and outside change windows, and recorded in the audit log.

**Daily report (`kubesage report`).** A short summary of each cluster: health, top issues and certificates expiring within 14 days.
Without flags it prints to the terminal; `--post` sends it to a Slack or Microsoft Teams incoming webhook. Keep the URL out of the config:
```yaml
reports:
  webhook_env: KUBESAGE_WEBHOOK   # export KUBESAGE_WEBHOOK=https://hooks.slack.com/services/…
  contexts: ['-dev$', 'staging']   # default: the current context
```
```bash
# crontab: every weekday at 08:30
30 8 * * 1-5  KUBESAGE_WEBHOOK=https://hooks.slack.com/services/… kubesage report --post
```
In the UI, `:daily-report` shows the same summary for the focused cluster and `:daily-report post` sends it (after asking).

**Share your screen read-only (`:share`).** Starts a small web server that shows exactly what your kubesage window shows, refreshed every
second, for pairing during an incident. Viewers cannot type or change anything. `:share` listens on this computer only; `:share lan [port]`
listens on your network so colleagues can open the link. The link contains a random token; anyone with the link can watch until
`:share stop`. The shared view pauses while a secret is revealed.

**Performance on big clusters.** kubesage's own report screens (certificates, images, right-sizing, storage, fleet …) query a lot, so they
refresh every 30 seconds instead of every 3 (`r` refreshes now; the status bar shows the interval). Change it with `slow_refresh_seconds`.

## Tests

```bash
make test
KUBESAGE_LIVE_OLLAMA=1 go test ./internal/ai -run Live -v       # hits a local Ollama
KUBESAGE_SNAPSHOT_DIR=/tmp/snap go test ./internal/ui -run Snapshot  # dumps ANSI screens
```

End-to-end tests run exec, port-forward, apply, scale schedules, a chaos test and a drain plan against a real
[kind](https://kind.sigs.k8s.io) cluster (CI does this on every push). They skip unless the current context starts with `kind-`:
```bash
kind create cluster --name kubesage-e2e
go test -tags e2e -v ./e2e/
```

## Documentation site and demo

The [documentation site](https://gauravthakur02.github.io/kubesage/) is built from this README by `docs/build.py` (one page per
section) and published by the `docs` workflow. Preview it locally with `pip install mkdocs-material && python docs/build.py && mkdocs serve`.
`docs/demo.tape` records the demo GIF with [VHS](https://github.com/charmbracelet/vhs) against a throwaway kind cluster.

## Releases

Tag a version and push it; the release workflow runs GoReleaser and attaches archives for macOS, Linux (amd64, arm64, armv7) and
Windows (amd64, arm64), `.deb`/`.rpm`/`.apk` packages and `checksums.txt`, with install instructions in the release notes:

```bash
git tag v0.2.0 && git push origin v0.2.0
```

After the release, the workflow points the Homebrew formula (`Formula/kubesage.rb`, built from source) at the new tag and commits it,
so `brew upgrade kubesage` picks it up. A Scoop manifest is published once a `scoop-bucket` repository exists and the
`SCOOP_BUCKET_GITHUB_TOKEN` secret is set. CI runs the tests on Linux, macOS and Windows for every push and pull request.

**Optional: signed and notarized macOS archives.** Only needed if you want browser downloads to open without a warning; Homebrew,
the install script and `go install` already avoid it for free. The release signs and notarizes the macOS binaries as soon as these
repository secrets exist (it needs a paid [Apple Developer Program](https://developer.apple.com/programs/) membership; without them
the step is skipped):

| Secret | Value |
|---|---|
| `MACOS_SIGN_P12` | base64 of your *Developer ID Application* certificate exported from Keychain as `.p12` (`base64 -i cert.p12`) |
| `MACOS_SIGN_PASSWORD` | the `.p12` export password |
| `MACOS_NOTARY_ISSUER_ID` | App Store Connect → Users and Access → Integrations → API key issuer ID |
| `MACOS_NOTARY_KEY_ID` | that API key's ID |
| `MACOS_NOTARY_KEY` | base64 of the downloaded `AuthKey_<id>.p8` |

Signing runs on the Linux release runner, so no Mac is needed in CI.

## Roadmap ideas

A terminal-recorded demo, publishing the VS Code extension to the Marketplace, and more plugins in the catalogue.

## License

[MIT](LICENSE) © 2026 Gaurav Thakur
