// kubesage for VS Code: opens kubesage in an editor terminal. No dependencies.
const vscode = require('vscode');
const cp = require('child_process');

function settings() {
  const c = vscode.workspace.getConfiguration('kubesage');
  return { bin: c.get('path') || 'kubesage', readOnly: c.get('readOnly'), extra: c.get('extraArgs') || [] };
}

function quote(a) {
  return /^[A-Za-z0-9_./:=@%+,-]+$/.test(a) ? a : JSON.stringify(a);
}

function run(args, name) {
  const s = settings();
  const all = [...s.extra, ...(s.readOnly && !args.includes('--readonly') ? ['--readonly'] : []), ...args];
  const term = vscode.window.createTerminal({ name: name || 'kubesage', location: vscode.TerminalLocation.Editor });
  term.show();
  term.sendText([s.bin, ...all].map(quote).join(' '));
}

function contexts() {
  return new Promise((resolve) => {
    cp.execFile(settings().bin, ['__complete', 'contexts'], { timeout: 5000 }, (err, out) => {
      resolve(err ? [] : out.split(/\r?\n/).filter(Boolean));
    });
  });
}

function activate(ctx) {
  ctx.subscriptions.push(
    vscode.commands.registerCommand('kubesage.open', () => run([])),
    vscode.commands.registerCommand('kubesage.openReadOnly', () => run(['--readonly'], 'kubesage (read-only)')),
    vscode.commands.registerCommand('kubesage.openContext', async () => {
      const list = await contexts();
      if (!list.length) {
        vscode.window.showErrorMessage('No kube contexts found (is kubesage on your PATH? see the kubesage.path setting)');
        return;
      }
      const pick = await vscode.window.showQuickPick(list, { placeHolder: 'Cluster to open' });
      if (pick) run(['--context', pick], 'kubesage · ' + pick);
    }),
    vscode.commands.registerCommand('kubesage.applyFile', (uri) => {
      const file = (uri && uri.fsPath) || (vscode.window.activeTextEditor && vscode.window.activeTextEditor.document.fileName);
      if (!file) return;
      run(['--cmd', ':apply ' + file], 'kubesage · apply');
    }),
    vscode.commands.registerCommand('kubesage.devFolder', (uri) => {
      if (!uri) return;
      run(['--cmd', ':dev ' + uri.fsPath], 'kubesage · dev');
    })
  );
}

function deactivate() {}

module.exports = { activate, deactivate };
