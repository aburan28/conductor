// Conductor Agent Sessions — VS Code companion to the `conductor` CLI.
//
// VS Code cannot be told to open an integrated terminal from the command line, so
// `conductor resume` fires a vscode:// URI at this extension when a paused session lived in
// a VS Code integrated terminal. The URI carries only a record id — never a command line —
// and this extension reads the actual session record from Conductor's own state directory.
// A URI that carried a command would let any page rendering a vscode:// link inject one; a
// record id is useless to anyone who cannot already write the user's 0600 record files.
//
// Plain JavaScript on purpose: no compile step, no toolchain in a Go repository. Package
// with `npx @vscode/vsce package` and install the .vsix with `code --install-extension`.

const vscode = require('vscode');
const cp = require('child_process');
const fs = require('fs');
const os = require('os');
const path = require('path');

function activate(context) {
  context.subscriptions.push(
    vscode.window.registerUriHandler({ handleUri }),
    vscode.commands.registerCommand('conductor.pauseAll', () => runConductor(['pause'])),
    vscode.commands.registerCommand('conductor.resumeAll', () => runConductor(['resume'])),
  );
}

// handleUri answers vscode://conductor.conductor-sessions/resume?id=<record-id>.
function handleUri(uri) {
  if (uri.path !== '/resume') {
    return;
  }
  const id = scrubId(new URLSearchParams(uri.query).get('id') || '');
  if (!id) {
    return;
  }
  const file = path.join(sessionsDir(), id + '.json');

  let rec;
  try {
    rec = JSON.parse(fs.readFileSync(file, 'utf8'));
  } catch {
    // Already resumed elsewhere, or the record never existed. Saying so beats silence:
    // the user clicked resume and something should acknowledge it.
    vscode.window.showInformationMessage('Conductor: that session is already resumed (or its record is gone).');
    return;
  }

  const terminal = vscode.window.createTerminal({
    name: (rec.harness || 'agent') + ' · conductor',
    cwd: rec.cwd || undefined,
  });
  terminal.show();
  terminal.sendText(relaunchCommand(rec), true);

  // Ownership protocol with the CLI: `conductor resume` leaves the record in place so an
  // unhandled handoff stays retryable; the record is deleted only here, once the terminal
  // actually exists. A relaunched `conductor wrap` writes a fresh record of its own.
  try {
    fs.unlinkSync(file);
  } catch {
    // Losing the delete only means the next `conductor pause` prunes a dead record.
  }
}

// relaunchCommand mirrors relaunchArgv in cmd/conductor/pause.go: the harness's own resume
// invocation, under `conductor wrap` (with the original capability flags) when the session
// was wrapped, bare when it was not.
function relaunchCommand(rec) {
  const tail = (rec.resume_args && rec.resume_args.length) ? rec.resume_args : (rec.args || []);
  let argv;
  if (rec.wrapped) {
    argv = [cliPath(), 'wrap', ...(rec.wrap_flags || []), rec.harness, ...tail];
  } else {
    argv = [rec.command || rec.harness, ...tail];
  }
  return argv.map(shellQuote).join(' ');
}

function runConductor(args) {
  cp.execFile(cliPath(), args, { timeout: 60000 }, (err, stdout, stderr) => {
    if (err) {
      vscode.window.showErrorMessage(
        'conductor ' + args.join(' ') + ' failed: ' + ((stderr || err.message || '').trim()));
      return;
    }
    const lines = (stdout || '').trim().split('\n').filter(Boolean);
    vscode.window.showInformationMessage('Conductor: ' + (lines.pop() || 'done.'));
  });
}

function sessionsDir() {
  const configured = vscode.workspace.getConfiguration('conductor').get('stateDir');
  const base = configured || process.env.CONDUCTOR_STATE_DIR || path.join(os.homedir(), '.conductor');
  return path.join(base, 'sessions');
}

function cliPath() {
  return vscode.workspace.getConfiguration('conductor').get('cliPath') || 'conductor';
}

// scrubId applies the same mapping as the CLI's record file naming (localstate.fileName):
// anything outside [A-Za-z0-9-] becomes '_'. The id arrives from a URI, and an id must never
// be able to name a path outside the sessions directory.
function scrubId(id) {
  return id.replace(/[^A-Za-z0-9-]/g, '_');
}

// shellQuote matches the CLI's quoting: bare when safe, single-quoted otherwise.
function shellQuote(s) {
  if (s === '') {
    return "''";
  }
  if (!/[ \t\n"'\\$&|;<>()*?\[\]#~`!{}]/.test(s)) {
    return s;
  }
  return "'" + s.replace(/'/g, "'\\''") + "'";
}

function deactivate() {}

module.exports = { activate, deactivate };
