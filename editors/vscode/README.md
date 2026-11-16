# kubesage for VS Code

Opens [kubesage](https://github.com/gauravthakur02/kubesage) in an editor tab.

| Command | What it does |
|---|---|
| **kubesage: Open** (`Cmd/Ctrl+Alt+K`) | kubesage on the current context |
| **kubesage: Open cluster…** | pick a kube context first |
| **kubesage: Open read-only** | start with every change disabled |
| **kubesage: Diff and apply this manifest** (right-click a `.yaml`) | diff against the cluster (`--cmd ":apply <file>"`), apply only after you confirm |
| **kubesage: Dev loop on this folder** (right-click a folder) | re-apply on every save into the current namespace (`--cmd ":dev <folder>"`) |

Settings: `kubesage.path`, `kubesage.readOnly`, `kubesage.extraArgs`.

Build a `.vsix` to install locally:
```bash
cd editors/vscode && npx @vscode/vsce package && code --install-extension kubesage-*.vsix
```
