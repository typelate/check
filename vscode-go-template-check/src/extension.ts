import * as vscode from 'vscode';
import * as cp from 'child_process';
import * as path from 'path';

let diagnostics: vscode.DiagnosticCollection;
let checkTimer: ReturnType<typeof setTimeout> | undefined;

// LINE_RE matches a single check-templates stderr line:
//   path/to/file.gohtml:line:col: message (E001)
// On Windows, absolute paths start with a drive letter (C:\...) which
// contains a colon, so we use a non-greedy match for the filename.
const LINE_RE = /^(.+?):(\d+):(\d+): (.+)$/;

// Template file extensions we trigger on (besides .go).
const TEMPLATE_EXT_RE = /\.(gohtml|tmpl|gotmpl|html)$/;

// Debounce delay (ms) for filesystem watcher events to batch rapid writes.
const DEBOUNCE_MS = 1500;

export function activate(context: vscode.ExtensionContext): void {
	diagnostics = vscode.languages.createDiagnosticCollection('templatecheck');
	context.subscriptions.push(diagnostics);

	checkBinaryInstalled();

	// Trigger on manual save.
	context.subscriptions.push(
		vscode.workspace.onDidSaveTextDocument(onSave)
	);

	// Trigger on filesystem changes (covers AI agents writing files directly).
	const goWatcher = vscode.workspace.createFileSystemWatcher('**/*.go');
	const tmplWatcher = vscode.workspace.createFileSystemWatcher('**/*.{gohtml,tmpl,gotmpl,html}');
	for (const watcher of [goWatcher, tmplWatcher]) {
		context.subscriptions.push(watcher);
		watcher.onDidChange(() => debouncedCheck());
		watcher.onDidCreate(() => debouncedCheck());
		watcher.onDidDelete(() => debouncedCheck());
	}
}

export function deactivate(): void {
	diagnostics?.dispose();
	if (checkTimer) {
		clearTimeout(checkTimer);
	}
}

function onSave(doc: vscode.TextDocument): void {
	if (doc.languageId === 'go' || TEMPLATE_EXT_RE.test(doc.fileName)) {
		runCheck();
	}
}

function debouncedCheck(): void {
	if (checkTimer) {
		clearTimeout(checkTimer);
	}
	checkTimer = setTimeout(() => {
		checkTimer = undefined;
		runCheck();
	}, DEBOUNCE_MS);
}

function checkBinaryInstalled(): void {
	const bin = getConfig().binaryPath;
	cp.exec(`"${bin}" --help`, { timeout: 5000 }, (err) => {
		if (err) {
			vscode.window.showInformationMessage(
				`go-template-check: '${bin}' not found on PATH. Install it?`,
				'Install'
			).then((choice) => {
				if (choice === 'Install') {
					const term = vscode.window.createTerminal('Go Template Check Install');
					term.sendText('go install github.com/typelate/check/cmd/check-templates@latest');
					term.show();
				}
			});
		}
	});
}

function runCheck(): void {
	const cfg = getConfig();
	const folder = vscode.workspace.workspaceFolders?.[0]?.uri.fsPath;
	if (!folder) {
		return;
	}

	const args: string[] = [];
	if (cfg.enableWarnings) {
		args.push('-w');
	}
	args.push('./...');

	const proc = cp.spawn(cfg.binaryPath, args, { cwd: folder });

	let stderr = '';
	proc.stderr.on('data', (chunk: Buffer) => {
		stderr += chunk.toString();
	});

	proc.on('error', (err) => {
		vscode.window.showErrorMessage(
			`go-template-check: failed to run '${cfg.binaryPath}': ${err.message}`
		);
	});

	proc.on('close', () => {
		diagnostics.clear();
		if (stderr.trim()) {
			applyDiagnostics(stderr, folder);
		}
	});
}

function applyDiagnostics(stderr: string, root: string): void {
	const byUri = new Map<string, vscode.Diagnostic[]>();

	for (const rawLine of stderr.split('\n')) {
		const line = rawLine.trim();
		if (!line) {
			continue;
		}
		const m = LINE_RE.exec(line);
		if (!m) {
			continue;
		}
		const [, file, lineStr, colStr, msg] = m;

		// Resolve relative paths against the workspace root.
		const absPath = path.isAbsolute(file) ? file : path.join(root, file);

		const severity = /\(E\d+\)$/.test(msg)
			? vscode.DiagnosticSeverity.Error
			: vscode.DiagnosticSeverity.Warning;

		const ln = Math.max(0, parseInt(lineStr, 10) - 1);
		const col = Math.max(0, parseInt(colStr, 10) - 1);

		// End column: extend to end of line so the squiggle covers the token.
		// We use a large number; VS Code clips it to the actual line length.
		const range = new vscode.Range(ln, col, ln, col + 200);

		const diag = new vscode.Diagnostic(range, msg, severity);
		diag.source = 'templatecheck';

		// Extract the diagnostic code (E001, W003, etc.) from the message suffix.
		const codeMatch = /\(((?:E|W)\d+)\)$/.exec(msg);
		if (codeMatch) {
			diag.code = codeMatch[1];
		}

		const key = vscode.Uri.file(absPath).toString();
		if (!byUri.has(key)) {
			byUri.set(key, []);
		}
		byUri.get(key)!.push(diag);
	}

	for (const [key, diags] of byUri) {
		diagnostics.set(vscode.Uri.parse(key), diags);
	}
}

function getConfig(): { binaryPath: string; enableWarnings: boolean } {
	const c = vscode.workspace.getConfiguration('templatecheck');
	return {
		binaryPath: c.get<string>('binaryPath', 'check-templates'),
		enableWarnings: c.get<boolean>('enableWarnings', true),
	};
}
