# kubeweave for VS Code

Opens [kubeweave](https://github.com/gauravthakur02/kubeweave) in an editor tab.

| Command | What it does |
|---|---|
| **kubeweave: Open** (`Cmd/Ctrl+Alt+K`) | kubeweave on the current context |
| **kubeweave: Open cluster…** | pick a kube context first |
| **kubeweave: Open read-only** | start with every change disabled |
| **kubeweave: Diff and apply this manifest** (right-click a `.yaml`) | diff against the cluster (`--cmd ":apply <file>"`), apply only after you confirm |
| **kubeweave: Dev loop on this folder** (right-click a folder) | re-apply on every save into the current namespace (`--cmd ":dev <folder>"`) |

Settings: `kubeweave.path`, `kubeweave.readOnly`, `kubeweave.extraArgs`.

Build a `.vsix` to install locally:
```bash
cd editors/vscode && npx @vscode/vsce package && code --install-extension kubeweave-*.vsix
```
