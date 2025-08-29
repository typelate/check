# Check

**Check** is a Go library for statically type-checking Go `text/template` and `html/template` usage.  
It analyzes template parse trees against Go types using the standard `go/types` package.

The library was originally developed as a backend for [`muxt check`](https://github.com/crhntr/muxt),
a CLI tool for generating HTTP handler functions from HTML templates.
The check package is encapsulated in the `muxt check` command.
Consider using `muxt check` directly.

> **Note:** The API may change in future releases. No guarantees of long-term stability are provided.

## Overview

Check works by walking a template’s parse tree and validating expressions against Go types.  
It requires:
- A `types.Type` that represents the data bound to `{{.}}` in the template.
- A `TreeFinder` to resolve named templates.
- A `CallChecker` to handle function calls (a default `Functions` type is provided).
- A `*types.Package` and `*token.FileSet` for type context.

The main entry point is:

```go
func ParseTree(global *Global, tree *parse.Tree, data types.Type) error
````

## Key Types

* **`Global`**
  Holds type and template resolution state. Constructed with `NewGlobal`.

* **`ParseTree`**
  Entry point to validate a template tree against a given `types.Type`.

* **`TreeFinder` / `FindTreeFunc`**
  Resolves other templates by name (wrapping `Template.Lookup`).

* **`Functions`**
  A set of callable template functions. Implements `CallChecker`.

   * Use `DefaultFunctions(pkg *types.Package)` to get the standard built-ins.
   * Extend with `Functions.Add`.

* **`CallChecker`**
  Interface for validating function calls within templates.

## Limitations

1. **Type required**
   You must provide a `types.Type` that represents the template’s root context (`.`).

2. **Function sets**
   Currently, default functions do not differentiate between `text/template` and `html/template`.

3. **Third-party template packages**
   Compatibility with specialized template libraries (e.g. [safehtml](https://pkg.go.dev/github.com/google/safehtml)) has not been fully tested.

4. **Runtime-only errors**
   ParseTree checks static type consistency but cannot detect runtime conditions such as out-of-range indexes.
