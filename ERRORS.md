# check error reporting — guide for LSP integration

This document describes how `github.com/typelate/check` (current as of
v0.3.0-dev.3) captures template type-checking errors and how a tool (such
as a language server) should consume and format them. The module requires
Go 1.26 (examples below use `errors.AsType`).

## How errors are captured

`check.Execute(global, tree, dataType)` and `check.Package(pkg, ...)` return
the built-in `error` type, but any non-nil result is always a
`*check.Error`. Recover it with:

```go
if checkErr, ok := errors.AsType[*check.Error](err); ok { ... }
```

`check.Error` is a **tree**:

- A **leaf** describes one failure: where it happened, what kind it is, and
  the Go type involved.
- An **aggregate** (`Type == check.ErrorTypeAggregate`) groups independent
  child failures found in the same subtree. The root returned by `Execute`
  is an aggregate whenever more than one failure exists; with exactly one
  failure the leaf itself is returned.

The checker does **not** stop at the first error. List items, `if`/`with`/
`range` pipelines and both branches, `template` invocations, and each
command argument are checked independently and all failures collected. The
walk only skips a subtree when a failure makes it untypeable: a failed
`with`/`template` pipeline skips that body (dot is unknown), and a failed
`range` pipeline skips body and else. One known cascade: a variable declared
in a failing pipeline (`{{$x := .Missing}}` or `{{if $x := .Missing}}`)
reports a follow-on "variable not found" for later uses of `$x` — those
follow-on leaves are marked `Secondary: true` so diagnostic tools can
suppress or de-emphasize them.

### Fields on a leaf `*check.Error`

| Field | Type | Meaning |
|---|---|---|
| `Type` | `check.ErrorType` | classification (see table below) |
| `Tree` | `*parse.Tree` | the template tree the failure was found in |
| `Node` | `parse.Node` | the exact offending node |
| `X` | `types.Type` | the most relevant Go type: the receiver for field/method lookups, the pipeline result for `range`, the callee signature for call errors; may be nil |
| `Decl` | `token.Position` | where the involved Go declaration is defined: the receiver type for lookups, the method for signature failures; zero when unknown. This field is the **only** place the declaration position lives — `Error()` messages are deterministic and never embed it — so use it for LSP `RelatedInformation` (URI + range), or append it textually at render time the way `check-templates` does |
| `Secondary` | `bool` | true for follow-on failures whose root cause is another error in the tree (see the cascade note above) |

Aggregates have `Type == ErrorTypeAggregate`, may carry the enclosing
`Tree`/`Node`, and expose their children via `Unwrap()`/`All`.

### Traversal

- `for e := range checkErr.All { ... }` — `All` is an `iter.Seq[*check.Error]`
  that walks the tree depth-first, pre-order, starting with the receiver.
  For diagnostics, iterate `All` and skip `ErrorTypeAggregate` nodes; every
  remaining element is one reportable failure with `Tree` and `Node` set.
- `Unwrap() []error` — exposes the wrapped cause on a leaf and the children
  on an aggregate, so `errors.Is`/`errors.As` traverse the whole tree.

### Positions for diagnostics

Each leaf's `Node.Position()` is a `parse.Pos`: a **byte offset into the
template source text** of `Tree`. `Tree.ErrorContext(Node)` returns a
`location` string of the form `file:line:col` (the file name comes from the
tree's parse name) plus the node's text. For an LSP `Range`, use the byte
offset as the start and `len(Node.String())` as an approximate span, or
re-derive line/column from the offset against the file content you already
hold in the editor.

### Structured causes

Two exported cause types carry extra structure; fetch them from a leaf with
`errors.As`:

- `*check.IdentifierError` — field/method/variable lookup failures.
  Fields: `Identifier string`, `Type types.Type` (the receiver being
  navigated), `Cause error`.
- `*check.CallError` — function/method call failures.
  Fields: `Name string`, `Signature *types.Signature`, `ArgTypes []types.Type`,
  `Cause error`.

### `ErrorType` classification

`ErrorType.String()` returns a stable slug suitable as an LSP diagnostic
`code`. Values:

| Constant | Slug | Reported when |
|---|---|---|
| `ErrorTypeUnknown` | `unknown` | error produced outside this package (custom `CallChecker`) |
| `ErrorTypeAggregate` | `aggregate` | groups child errors; not itself a failure |
| `ErrorTypeUnexpectedNode` | `unexpected-node` | parser node the checker cannot walk (internal) |
| `ErrorTypeVariableNotFound` | `variable-not-found` | undeclared template variable |
| `ErrorTypeConstantOverflow` | `constant-overflow` | numeric constant overflows int |
| `ErrorTypeTemplateNotFound` | `template-not-found` | `{{template "name"}}` target undefined |
| `ErrorTypeBadCommand` | `bad-command` | command that cannot be evaluated (e.g. `{{nil}}`) |
| `ErrorTypeNotAFunction` | `not-a-function` | arguments given to a non-function value |
| `ErrorTypeFieldNotExported` | `field-not-exported` | access to an unexported field or method |
| `ErrorTypeFieldOrMethodNotFound` | `field-or-method-not-found` | lookup failed on the receiver type |
| `ErrorTypeBadSignature` | `bad-signature` | method/function signature not callable from a template |
| `ErrorTypeCallArguments` | `call-arguments` | wrong number or types of call arguments (incl. builtins) |
| `ErrorTypeUnknownFunction` | `unknown-function` | call to an undefined function |
| `ErrorTypeRange` | `range` | `range` over a type that cannot be iterated |
| `ErrorTypeIdentifierChain` | `identifier-chain` | field chain through a type that does not support selection |
| `ErrorTypeMapKey` | `map-key` | map index whose key cannot match the map's key type |

## How to format output

### Compact: `Error() string`

Every error renders as a compact single line with no qualifier needed —
type names print with **full package paths** (`types.WriteType` with a nil
`types.Qualifier`). `Global.Qualifier` does not affect message text.

```
page.gohtml:1:2: executing "page.gohtml" at <.Missng>: field or method Missng not found on example.com/web.Page
```

An aggregate's `Error()` joins its children's lines with `\n` — one line
per failure. Suitable directly as an LSP diagnostic `message`.

### Detailed: `DetailedError(w io.Writer, q types.Qualifier) error`

For hovers and rich diagnostics. Writes one block per failure (blocks
separated by a blank line). Each block is the location line followed by
supporting detail:

- identifier lookup failures list the receiver type's exported fields
  (name and type, name-aligned, declaration order, embedded structs
  flattened) and its exported method signatures, one per line;
- call failures show the callee signature and the argument types.

**Every line — including the location line's message — renders type names
through `q`.** Build `q` from the consuming file's imports so types print
with the identifiers that file actually uses:

```go
// idents maps *types.Package -> local identifier from the file's import specs
q := func(p *types.Package) string {
    if name, ok := idents[p]; ok {
        return name // covers aliased imports
    }
    return p.Name()
}

var sb strings.Builder
// Returns the first write error; writes to a strings.Builder never fail.
_ = checkErr.DetailedError(&sb, q)
hover := sb.String()
```

Output with such a qualifier:

```
page.gohtml:1:2: executing "page.gohtml" at <.Missng>: field or method Missng not found on web.Page

web.Page has:
  Title string
  Owner web.User
  Visit(count int) string

page.gohtml:1:13: executing "page.gohtml" at <.SetOwner>: argument 0 has type string expected web.User

  signature: SetOwner(owner web.User) string
  arguments:
    [0] string
```

Passing `q = nil` prints full package paths, matching `Error()`. A type
with no exported members renders `T has no exported fields or methods`.

#### Inline type elision

Unlike `Error()`, `DetailedError` elides massive inline (unnamed) types so
lines stay readable. An unnamed struct, interface, or signature whose full
rendering exceeds a small threshold (64 characters) prints as a summary:

- `struct{...}` / `interface{...}`
- signatures render with shortened, unnamed parameter and result types:
  `Configure(struct{...}) string`
- container shape is preserved: `[]struct{...}`, `map[string]struct{...}`,
  `*struct{...}`, `chan struct{...}`

Named types and short inline types always render in full. Elision is
presentation-only: the complete `types.Type` values remain on the leaf's
`X` field and on the `IdentifierError`/`CallError` causes, and `Error()`
still renders them in full — so a hover can show the summary while a
"go to type" or expanded view uses the real type.

Example — a lookup failure on a large anonymous root context:

```
detail.gohtml:1:2: executing "detail.gohtml" at <.Missing>: field or method Missing not found on struct{...}

struct{...} has:
  Title string
  Meta  struct{...}
```

### Recommended LSP wiring

```go
root, ok := errors.AsType[*check.Error](check.Execute(global, tree, dataType))
if !ok {
    return nil // no diagnostics
}
var diagnostics []Diagnostic
for e := range root.All {
    if e.Type == check.ErrorTypeAggregate {
        continue
    }
    var sb strings.Builder
    _ = e.DetailedError(&sb, q) // per-leaf call works too; e is a *check.Error
    diagnostics = append(diagnostics, Diagnostic{
        Range:    rangeFor(e.Tree, e.Node), // from Node.Position()
        Code:     e.Type.String(),
        Message:  e.Error(),                // compact, qualifier-free
        Severity: severityFor(e.Secondary), // e.g. Hint/Unnecessary for follow-ons
        // hover: sb.String()
        // RelatedInformation: e.Decl (the involved Go declaration), when valid
    })
}
```

### Legacy formatting (still available)

`check.FormatVerbose(err)` and the `VerboseErrorer` interface predate
`DetailedError`; they render the receiver type's Go source declaration
(with godoc) instead of a member listing and do not accept a qualifier.
The `check-templates` CLI uses them. New tooling should prefer
`DetailedError`.
