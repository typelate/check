# Check

**Check** is a Go library for statically type-checking `text/template` and `html/template`.  
It helps catch template/type mismatches early, making refactoring safer when changing types or templates.

To use it, call `ParseTree` and provide:
- a `types.Type` for the template’s data (`.`), and
- the template’s `parse.Tree`.

See [example_test.go](./example_test.go) for a working example.

Originally built as part of [`muxt`](https://github.com/crhntr/muxt),  
the package also powers the `muxt check` CLI command.  
If you just want command-line checks, consider using `muxt check` directly.

## Key Types and Functions

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
