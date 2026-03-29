# Check [![Go Reference](https://pkg.go.dev/badge/github.com/typelate/check.svg)](https://pkg.go.dev/github.com/typelate/check)

**Check** is a Go library and CLI for statically type-checking `text/template` and `html/template`. It catches template/type mismatches early, making refactoring safer when changing types or templates.

It includes a [CLI](#check-templates-cli) and a [VS Code extension](#vs-code-extension).

## `check-templates` CLI

If all your `ExecuteTemplate` or `Execute` calls use a static type for the data argument, you can use the CLI directly:

```sh
go get -tool github.com/typelate/check/cmd/check-templates
go tool check-templates ./...
```

Flags:
- `-v` &mdash; list each call with position, template name, and data type
- `-w` &mdash; enable warnings for potential issues (see [Warnings](#warnings) below)
- `-C dir` &mdash; change working directory before loading packages
- `-o format` &mdash; output format: `tsv` (default) or `jsonl`
- `--mcp` &mdash; run as an [MCP](https://modelcontextprotocol.io/) server over stdio (for AI agent integration)
- `--version` &mdash; print version and exit

### How the CLI discovers templates

The CLI works by statically analyzing your Go source code. It traces each `ExecuteTemplate` and `Execute` call back to the variable that holds the `*template.Template`, then follows that variable's initialization chain to find the template files. This means:

1. **`ExecuteTemplate` must use a string literal** for the template name (second argument). Calls that pass a variable or expression will produce a warning (with `-w`) and be skipped. **`Execute`** calls are also supported &mdash; the template name is inferred from the receiver's root template.

2. **Template initialization works best with static arguments.** File paths passed to `ParseFiles`, glob patterns passed to `ParseGlob`, and embed patterns passed to `ParseFS` are ideally string literals. However, the tool can also trace `embed.FS` variables through function parameters across packages, resolve `fs.Glob` calls against embedded file lists, and handle spread `[]string` variables and per-page template map construction.

3. **Supported initialization patterns:**
   - `template.Must(template.ParseFiles("a.html", "b.html"))`
   - `template.Must(template.ParseGlob("templates/*.html"))`
   - `template.Must(template.ParseFS(fs, "*.html"))`
   - `template.New("name").ParseFiles("a.html")`
   - Chained calls: `.Funcs(...)`, `.Option(...)`, `.Delims(...)`, `.Parse(...)`
   - Additional `.ParseFiles(...)`, `.ParseGlob(...)`, or `.ParseFS(...)` calls on an already-initialized template variable

## Warnings

The `-w` flag enables warnings for issues that are not type errors but may indicate bugs. All warnings are printed to stderr.

### Unguarded pointer dereference

When dot is a pointer type (e.g. `*Page`), accessing a field like `.Title` will panic at runtime if dot is nil. The tool warns unless the access is guarded by `{{with}}`, `{{if}}`, or the `and` short-circuit pattern.

```go
type Page struct { Title string }

func render(p *Page) {
    _ = templates.ExecuteTemplate(w, "index.gohtml", p)
}
```

**Warns** &mdash; accessing `.Title` on a pointer without a nil guard:
```
{{.Title}}
```

**OK** &mdash; guarded with `{{with}}`:
```
{{with .}}
  {{.Title}}
{{end}}
```

**OK** &mdash; guarded with `{{if}}`:
```
{{if .}}
  {{.Title}}
{{end}}
```

**OK** &mdash; guarded with `and` short-circuit (Go's `and` returns the first falsy value without evaluating the rest):
```
{{if and .User (eq .User.Role "admin")}}
  {{.User.Username}}
{{end}}
```

Guards also work through `$` references (`$.User`), sub-template calls (`{{template "nav" .}}`), and inside `{{range}}` blocks.

#### `templatecheck:"nonil"` struct tag

For pointer-typed struct fields that are always initialized before being passed to a template, you can suppress W003 with a struct tag:

```go
type PageData struct {
    Title string
    S     *Strings     `templatecheck:"nonil"`
    User  *models.User `templatecheck:"nonil"`
}
```

The tag is respected for direct access (`.S.AppName`), variable assignment (`$s := .S` then `$s.AppName`), `$` references (`$.User.Role`), and embedded structs.

### Interface field access

When dot is `interface{}` or `any`, field access cannot be verified at compile time.

```go
func render(data any) {
    _ = templates.ExecuteTemplate(w, "page.gohtml", data)
}
```

**Warns** &mdash; field access on an interface type:
```
{{.Title}}
```

### Unused templates

Templates loaded via `ParseFS`, `ParseFiles`, or `ParseGlob` that are never referenced by any `ExecuteTemplate` call or `{{template}}` action.

```go
//go:embed *.gohtml
var source embed.FS

var templates = template.Must(template.New("app").ParseFS(source, "*"))

func render() {
    _ = templates.ExecuteTemplate(w, "index.gohtml", data)
}
```

**Warns** if `unused.gohtml` exists in the embed but is never referenced:
```
main.go:5:5: template "unused.gohtml" is defined but never referenced (W002)
```

### Non-static ExecuteTemplate name

`ExecuteTemplate` must be called with a string literal for the template name. Calls with a variable or expression cannot be checked statically.

```go
// Warns — template name is a variable, not a string literal:
name := getTemplateName()
_ = templates.ExecuteTemplate(w, name, data)

// OK — template name is a string literal:
_ = templates.ExecuteTemplate(w, "index.gohtml", data)
```

### Unused variables

Variables declared with `$x := ...` that are never referenced in the template.

```
{{$x := .Title}}  {{/* $x is never used */}}
<h1>{{.Title}}</h1>
```

### Dead conditional branches

Branches with literal `true`, `false`, or `nil` conditions that can never execute.

```
{{if true}}always{{else}}never reached (W006){{end}}
{{if false}}never reached (W006){{end}}
```

### Inconsistent sub-template types

A sub-template invoked from multiple `{{template}}` call sites with incompatible data types.

```
{{template "header" .Page}}   {{/* passes Page */}}
{{template "header" .Count}}  {{/* passes int — W007 */}}
```

### Warning reference

| Code | Category |
|------|----------|
| W001 | Non-static `ExecuteTemplate` name |
| W002 | Unused template |
| W003 | Unguarded pointer dereference |
| W004 | Interface field access |
| W005 | Unused variable |
| W006 | Dead conditional branch |
| W007 | Inconsistent sub-template types |

## Errors

These are type errors that `check-templates` reports regardless of the `-w` flag:

### Field not found

Accessing a field that does not exist on the data type.

```go
type Page struct { Title string }
```

**Error:**
```
{{.Titel}}  {{/* typo — "Titel" does not exist on Page */}}
```

### Type mismatch in template calls

When `{{template "name" .}}` passes a type that doesn't match what the sub-template expects.

### Printf format mismatch

`{{printf "%d" .Name}}` where `.Name` is a string produces a type error. The tool validates that format verbs (`%d`, `%s`, `%f`, etc.) match the types of the corresponding arguments. `%v` accepts any type.

## VS Code extension

The `vscode-go-template-check` directory contains a VS Code extension that shows diagnostics **inside template files** (`.gohtml`, `.tmpl`, `.gotmpl`) &mdash; not just at Go call sites.

Features:
- Red squiggles on `{{.MissingField}}` in template files
- Yellow squiggles for warnings (W001&ndash;W007)
- Syntax highlighting for Go template directives in `.gohtml` files
- Runs automatically on save

The extension requires the `check-templates` binary on your `PATH`. See the [extension README](./vscode-go-template-check/README.md) for setup and configuration.

## Library usage

Call `Execute` with a `types.Type` for the template's data (`.`) and the template's `parse.Tree`. See [example_test.go](./example_test.go) for a working example.

## Related projects

- [`muxt`](https://github.com/typelate/muxt) &mdash; builds on this library to type-check templates wired to HTTP handlers. If you only need command-line checks, `muxt check` works too.
- [jba/templatecheck](https://github.com/jba/templatecheck) &mdash; a more mature alternative for template type-checking.

## Limitations

1. You must provide a `types.Type` for the template's root context (`.`).
2. No support for third-party template packages (e.g. [safehtml](https://pkg.go.dev/github.com/google/safehtml)).
3. Cannot detect runtime conditions such as out-of-range indexes or errors from boxed types.
4. Template initialization generally requires static arguments, but the tool can trace `embed.FS` variables through function parameters across packages and resolve `fs.Glob` patterns against embedded file lists. Dynamically constructed file lists that cannot be statically resolved are skipped gracefully.
