# Check [![Go Reference](https://pkg.go.dev/badge/github.com/typelate/check.svg)](https://pkg.go.dev/github.com/typelate/check)

**Check** is a Go library for statically type-checking `text/template` and `html/template`. It catches template/type mismatches early, making refactoring safer when changing types or templates.

## `check-templates` CLI

If all your `ExecuteTemplate` calls use a string literal for the template name and a static type for the data argument, you can use the CLI directly:

```sh
go get -tool github.com/typelate/check/cmd/check-templates
go tool check-templates ./...
```

Flags:
- `-v` &mdash; list each call with position, template name, and data type
- `-definitions` &mdash; list where each checked template was defined
- `-C dir` &mdash; change working directory before loading packages
- `-o format` &mdash; output format: `tsv` (default) or `jsonl`

## Library usage

Call `Execute` with a `types.Type` for the template's data (`.`) and the template's `parse.Tree`. See [example_test.go](./example_test.go) for a working example.

### Template definition positions

`Package` hands each inspector callback a `Definition` locating the template it is about to check. A `Definition` carries three `Span`s &mdash; the `{{define}}` or `{{block}}` clause, the matching `{{end}}`, and the quoted name literal inside the clause &mdash; along with the template's `parse.Tree`.

A `Span` embeds a `token.Position` and adds a byte `Length`, so it reports a file, line, column, offset and length with the same semantics as `go/token`. Note that `parse.Tree.ErrorContext` counts columns from zero instead, so the two differ by one.

Positions address real bytes in a real file. A template read by `ParseFS` resolves against the template file. A template written as a Go string literal resolves against the `.go` file holding it, with escape sequences accounted for, so `{{define \"x\"}}` reports the width of the escaped source rather than of the decoded text. A template file's own root template has no define clause, so it spans the whole file.

When a name is defined more than once, `Definition` reports the one that survived, applying the same rule `text/template` does: an empty definition does not displace one that already has a body.

## Related projects

- [`muxt`](https://github.com/typelate/muxt) &mdash; builds on this library to type-check templates wired to HTTP handlers. If you only need command-line checks, `muxt check` works too.
- [jba/templatecheck](https://github.com/jba/templatecheck) &mdash; a reflect based alternative for template type-checking maintained by a Go team member.

## Limitations

1. You must provide a `types.Type` for the template's root context (`.`).
2. No support for third-party template packages (e.g. [safehtml](https://pkg.go.dev/github.com/google/safehtml)).
3. Cannot detect runtime conditions such as out-of-range indexes or errors from boxed types.
