# Go Template Check — VS Code Extension

Type-checks Go `html/template` and `text/template` calls and reports errors as
squiggles **inside template files** (`.gohtml`, `.tmpl`, `.gotmpl`).

## Features

- **Type errors** — red squiggles on `{{.MissingField}}` in template files
- **Warnings** — yellow squiggles for unused variables (`W005`), dead branches
  (`W006`), inconsistent template types (`W007`), and more
- **Syntax highlighting** — `{{if}}`, `{{.Field}}`, `$var`, `{{/* comments */}}`
  highlighted in `.gohtml` files
- **On save** — runs automatically when any `.go` or template file is saved

## Requirements

The `check-templates` binary must be on your `PATH`. If it's not found, the
extension will prompt you to install it.

**Manual install:**

```sh
go install github.com/typelate/check/cmd/check-templates@latest
```

Go 1.21 or later is required.

## Supported file extensions

| Extension | Language ID | Notes |
|-----------|-------------|-------|
| `.gohtml` | `gohtml` | Registered by this extension; full syntax highlighting |
| `.tmpl`   | `gotmpl`  | Registered by vscode-go; diagnostics still applied |
| `.gotmpl` | `gotmpl`  | Registered by vscode-go; diagnostics still applied |

## Settings

| Setting | Default | Description |
|---------|---------|-------------|
| `templatecheck.binaryPath` | `"check-templates"` | Path to the binary |
| `templatecheck.enableWarnings` | `true` | Show W001–W007 warnings |

## Complementarity with gopls

This extension and gopls serve different roles:

- **gopls** — shows diagnostics at the **Go call site** (e.g. the
  `ExecuteTemplate` line in `main.go`). Useful in any editor via `go vet`.
- **This extension** — shows diagnostics inside the **template file itself**
  (e.g. `{{.Missing}}` in `index.gohtml`). Covers `.gohtml` files that gopls
  does not handle by default.

Both can be active at the same time for complementary coverage.

## Building from source

```sh
# Install esbuild (Go binary, no npm needed for compilation)
go install github.com/evanw/esbuild/cmd/esbuild@latest

# Compile the extension
cd vscode-go-template-check
esbuild src/extension.ts --bundle --platform=node --external:vscode --outfile=out/extension.js --sourcemap

# Package (requires npm for vsce)
npm install
npx @vscode/vsce package
```
