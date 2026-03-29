package check

import (
	"bytes"
	"fmt"
	"go/token"
	"go/types"
	"maps"
	"strconv"
	"strings"
	"text/template/parse"
)

type Error struct {
	Tree *parse.Tree
	Node parse.Node
	err  error
}

func newError(tree *parse.Tree, node parse.Node, message string, args ...any) *Error {
	loc, context := tree.ErrorContext(node)
	return &Error{
		Tree: tree,
		Node: node,
		err:  fmt.Errorf("%s: executing %q at <%s>: %s (E001)", loc, tree.Name, context, fmt.Sprintf(message, args...)),
	}
}

func wrapError(tree *parse.Tree, node parse.Node, err error) *Error {
	return &Error{
		Tree: tree,
		Node: node,
		err:  err,
	}
}

func (e *Error) Error() string {
	return e.err.Error()
}

func (e *Error) Unwrap() error {
	return e.err
}

// WarningFunc is called when a non-fatal issue is detected during
// type-checking, such as field access on an interface type or
// unguarded pointer dereference.
type WarningFunc func(category WarningCategory, tree *parse.Tree, node parse.Node, message string)

type Global struct {
	trees TreeFinder
	calls CallChecker

	pkg             *types.Package
	fileSet         *token.FileSet
	typeNodeMapping TypeNodeMapping

	InspectTemplateNode TemplateNodeInspectorFunc
	InspectCallNode     ExecuteTemplateNodeInspectorFunc
	Warn                WarningFunc

	// nonilFields caches struct fields tagged with `templatecheck:"nonil"`.
	// Keyed by the underlying *types.Struct; value is a set of field names.
	nonilFields map[*types.Struct]map[string]struct{}

	// Qualifier controls how types are printed in error messages.
	// If nil, types are printed with their full package path.
	// See types.WriteType for details.
	Qualifier types.Qualifier

	// subTemplateCallTypes records every data type a sub-template is
	// invoked with via {{template "name" .Data}}, keyed by template name.
	// Populated during Execute; used post-walk to detect W007.
	subTemplateCallTypes map[string][]types.Type
}

type TemplateNodeInspectorFunc func(node *parse.TemplateNode, t *parse.Tree, tp types.Type)

func NewGlobal(pkg *types.Package, fileSet *token.FileSet, trees TreeFinder, fnChecker CallChecker) *Global {
	return &Global{
		trees:                trees,
		calls:                fnChecker,
		pkg:                  pkg,
		fileSet:              fileSet,
		typeNodeMapping:      make(TypeNodeMapping),
		subTemplateCallTypes: make(map[string][]types.Type),
	}
}

// TypeString returns the string representation of typ using the configured Qualifier.
func (g *Global) TypeString(typ types.Type) string {
	var buf bytes.Buffer
	types.WriteType(&buf, typ, g.Qualifier)
	return buf.String()
}

// TreeFinder should wrap https://pkg.go.dev/html/template#Template.Lookup and return the Tree field from the Template
// If you are using text/template the lookup function from that package should also work.
type TreeFinder interface {
	FindTree(name string) (*parse.Tree, bool)
}

type FindTreeFunc func(name string) (*parse.Tree, bool)

func (fn FindTreeFunc) FindTree(name string) (*parse.Tree, bool) {
	return fn(name)
}

type CallChecker interface {
	CheckCall(*Global, string, []parse.Node, []types.Type) (types.Type, error)
}

type TypeNodeMapping map[types.Type][]parse.Node

func Execute(global *Global, tree *parse.Tree, data types.Type) error {
	s := &scope{
		global: global,
		variables: map[string]types.Type{
			"$": data,
		},
		guarded:   make(map[string]struct{}),
		nonilVars: make(map[string]struct{}),
		declared:  make(map[string]parse.Node),
		used:      make(map[string]struct{}),
	}
	_, err := s.walk(tree, data, nil, tree.Root)
	s.warnUnused(tree)
	return err
}

type scope struct {
	global    *Global
	variables map[string]types.Type
	guarded   map[string]struct{} // set of field paths known non-nil (e.g. ".Foo.Bar")
	nonilVars map[string]struct{} // template variables known non-nil (e.g. "$s")
	declared  map[string]parse.Node // variables declared in this scope (name → declaration node)
	used      map[string]struct{}   // variables read in this scope or child scopes

	// resultNonil is set to true by checkIdentifiers when the final field
	// in a chain has a templatecheck:"nonil" struct tag. Read and cleared
	// by checkPipeNode to propagate nonil status to declared variables.
	resultNonil bool
}

func (s *scope) child() *scope {
	c := &scope{
		global:    s.global,
		variables: maps.Clone(s.variables),
		guarded:   make(map[string]struct{}, len(s.guarded)),
		nonilVars: maps.Clone(s.nonilVars),
		declared:  make(map[string]parse.Node),
		used:      s.used, // share with parent so child uses bubble up
	}
	for k, v := range s.guarded {
		c.guarded[k] = v
	}
	return c
}

// warnUnused emits W005 warnings for variables declared in this scope
// that were never read.
func (s *scope) warnUnused(tree *parse.Tree) {
	if s.global.Warn == nil {
		return
	}
	for name, node := range s.declared {
		if _, ok := s.used[name]; !ok {
			s.global.Warn(WarnUnusedVariable, tree, node, fmt.Sprintf("variable %s declared but not used", name))
		}
	}
}

// pipeFieldPath extracts the field path from a pipe expression.
// For {{with .Foo.Bar}}, returns ".Foo.Bar".
// For {{with .}}, returns ".".
// Returns "" if the pipe is not a simple field or dot expression.
func pipeFieldPath(pipe *parse.PipeNode) string {
	if pipe == nil || len(pipe.Cmds) != 1 {
		return ""
	}
	cmd := pipe.Cmds[0]
	if len(cmd.Args) != 1 {
		return ""
	}
	switch n := cmd.Args[0].(type) {
	case *parse.DotNode:
		return "."
	case *parse.FieldNode:
		return "." + strings.Join(n.Ident, ".")
	case *parse.VariableNode:
		// Handle $-prefixed paths like $.User → "$.User"
		if len(n.Ident) >= 2 && n.Ident[0] == "$" {
			return "$." + strings.Join(n.Ident[1:], ".")
		}
	}
	return ""
}

// pipeAndGuardPaths extracts guard paths from "and" chains in a pipe.
// For {{if and .User (eq .User.Role "admin")}}, returns [".User"].
// All bare variable/field arguments in the and chain contribute paths.
func pipeAndGuardPaths(pipe *parse.PipeNode) []string {
	if pipe == nil || len(pipe.Cmds) != 1 {
		return nil
	}
	cmd := pipe.Cmds[0]
	if len(cmd.Args) < 3 {
		return nil
	}
	// Check if the first arg is the "and" identifier.
	ident, ok := cmd.Args[0].(*parse.IdentifierNode)
	if !ok || ident.Ident != "and" {
		return nil
	}
	// Collect guard paths from all arguments (the bare references).
	var paths []string
	for _, arg := range cmd.Args[1:] {
		if p := nodeGuardPath(arg); p != "" {
			paths = append(paths, p)
		}
	}
	return paths
}

func (s *scope) walk(tree *parse.Tree, dot, prev types.Type, node parse.Node) (types.Type, error) {
	switch n := node.(type) {
	case *parse.DotNode:
		return dot, nil
	case *parse.ListNode:
		return nil, s.checkListNode(tree, dot, prev, n)
	case *parse.ActionNode:
		return nil, s.checkActionNode(tree, dot, prev, n)
	case *parse.CommandNode:
		return s.checkCommandNode(tree, dot, prev, n)
	case *parse.FieldNode:
		return s.checkFieldNode(tree, dot, n, nil)
	case *parse.PipeNode:
		return s.checkPipeNode(tree, dot, n)
	case *parse.IfNode:
		return nil, s.checkIfNode(tree, dot, n)
	case *parse.RangeNode:
		return nil, s.checkRangeNode(tree, dot, n)
	case *parse.TemplateNode:
		return nil, s.checkTemplateNode(tree, dot, n)
	case *parse.BoolNode:
		return types.Typ[types.Bool], nil
	case *parse.StringNode:
		return types.Typ[types.String], nil
	case *parse.NumberNode:
		return newNumberNodeType(tree, n)
	case *parse.VariableNode:
		return s.checkVariableNode(tree, n, nil)
	case *parse.IdentifierNode:
		return s.checkIdentifierNode(tree, n)
	case *parse.TextNode:
		return nil, nil
	case *parse.WithNode:
		return nil, s.checkWithNode(tree, dot, n)
	case *parse.CommentNode:
		return nil, nil
	case *parse.NilNode:
		return types.Typ[types.UntypedNil], nil
	case *parse.ChainNode:
		return s.checkChainNode(tree, dot, prev, n, nil)
	case *parse.BranchNode:
		return nil, nil
	case *parse.BreakNode:
		return nil, nil
	case *parse.ContinueNode:
		return nil, nil
	default:
		return nil, newError(tree, n, "missing node type check %T", n)
	}
}

func (s *scope) checkChainNode(tree *parse.Tree, dot, prev types.Type, n *parse.ChainNode, args []types.Type) (types.Type, error) {
	x, err := s.walk(tree, dot, prev, n.Node)
	if err != nil {
		return nil, err
	}
	return s.checkIdentifiers(tree, x, n, n.Field, args)
}

func (s *scope) checkVariableNode(tree *parse.Tree, n *parse.VariableNode, args []types.Type) (types.Type, error) {
	s.used[n.Ident[0]] = struct{}{}
	tp, ok := s.variables[n.Ident[0]]
	if !ok {
		return nil, newError(tree, n, "variable %s not found", n.Ident[0])
	}
	// If this variable is known non-nil (from a templatecheck:"nonil"
	// struct tag), temporarily guard "." so the first pointer deref in
	// the identifier chain is not flagged as W003.
	if _, nonil := s.nonilVars[n.Ident[0]]; nonil {
		_, alreadyGuarded := s.guarded["."]
		s.guarded["."] = struct{}{}
		if !alreadyGuarded {
			defer delete(s.guarded, ".")
		}
	}
	// For $ references, translate $.-prefixed guarded paths to .-prefixed
	// form so checkIdentifiers can match them. For example, if {{if $.User}}
	// guards "$.User", translate to ".User" for the identifier chain check.
	if n.Ident[0] == "$" {
		var added []string
		for path := range s.guarded {
			if strings.HasPrefix(path, "$.") {
				dotPath := path[1:] // "$.User" → ".User"
				if _, exists := s.guarded[dotPath]; !exists {
					s.guarded[dotPath] = struct{}{}
					added = append(added, dotPath)
				}
			}
		}
		if len(added) > 0 {
			defer func() {
				for _, p := range added {
					delete(s.guarded, p)
				}
			}()
		}
	}
	return s.checkIdentifiers(tree, tp, n, n.Ident[1:], args)
}

func (s *scope) checkListNode(tree *parse.Tree, dot, prev types.Type, n *parse.ListNode) error {
	for _, child := range n.Nodes {
		if _, err := s.walk(tree, dot, prev, child); err != nil {
			return err
		}
	}
	return nil
}

func (s *scope) checkActionNode(tree *parse.Tree, dot, prev types.Type, n *parse.ActionNode) error {
	_, err := s.walk(tree, dot, prev, n.Pipe)
	return err
}

func (s *scope) checkPipeNode(tree *parse.Tree, dot types.Type, n *parse.PipeNode) (types.Type, error) {
	var result types.Type
	for _, cmd := range n.Cmds {
		tp, err := s.walk(tree, dot, result, cmd)
		if err != nil {
			return nil, err
		}
		result = tp
	}
	if len(n.Decl) > 0 && len(n.Decl[0].Ident) > 0 {
		name := n.Decl[0].Ident[0]
		s.variables[name] = result
		if s.resultNonil {
			s.nonilVars[name] = struct{}{}
			s.resultNonil = false
		}
		if name != "$" {
			s.declared[name] = n.Decl[0]
		}
	}
	return result, nil
}

// deadBranchKind inspects a pipe to see if it is a literal constant condition.
// Returns "true", "false", or "nil" if the pipe is a single literal BoolNode
// or NilNode; otherwise returns "".
func deadBranchKind(pipe *parse.PipeNode) string {
	if pipe == nil || len(pipe.Cmds) != 1 {
		return ""
	}
	cmd := pipe.Cmds[0]
	if len(cmd.Args) != 1 {
		return ""
	}
	switch n := cmd.Args[0].(type) {
	case *parse.BoolNode:
		if n.True {
			return "true"
		}
		return "false"
	}
	return ""
}

func (s *scope) checkIfNode(tree *parse.Tree, dot types.Type, n *parse.IfNode) error {
	_, err := s.walk(tree, dot, nil, n.Pipe)
	if err != nil {
		return err
	}
	// Warn about literal-constant conditions.
	if s.global.Warn != nil {
		switch deadBranchKind(n.Pipe) {
		case "true":
			if n.ElseList != nil {
				s.global.Warn(WarnDeadBranch, tree, n.Pipe, "else branch is unreachable: condition is always true")
			}
		case "false":
			s.global.Warn(WarnDeadBranch, tree, n.Pipe, "if branch is unreachable: condition is always false")
		}
	}
	ifScope := s.child()
	if path := pipeFieldPath(n.Pipe); path != "" {
		ifScope.guarded[path] = struct{}{}
	}
	// Extract additional guard paths from "and" chains in the pipe.
	for _, path := range pipeAndGuardPaths(n.Pipe) {
		if strings.HasPrefix(path, "$") && !strings.Contains(path, ".") {
			// Bare variable like "$u" — mark as nonil so checkVariableNode
			// guards the first pointer deref.
			ifScope.nonilVars[path] = struct{}{}
		} else {
			ifScope.guarded[path] = struct{}{}
		}
	}
	if _, err := ifScope.walk(tree, dot, nil, n.List); err != nil {
		return err
	}
	ifScope.warnUnused(tree)
	if n.ElseList != nil {
		elseScope := s.child()
		if _, err := elseScope.walk(tree, dot, nil, n.ElseList); err != nil {
			return err
		}
		elseScope.warnUnused(tree)
	}
	return nil
}

func (s *scope) checkWithNode(tree *parse.Tree, dot types.Type, n *parse.WithNode) error {
	child := s.child()
	x, err := child.walk(tree, dot, nil, n.Pipe)
	if err != nil {
		return err
	}
	child.warnUnused(tree)
	// Warn about literal-constant conditions.
	if s.global.Warn != nil {
		switch deadBranchKind(n.Pipe) {
		case "true":
			if n.ElseList != nil {
				s.global.Warn(WarnDeadBranch, tree, n.Pipe, "else branch is unreachable: condition is always true")
			}
		case "false":
			s.global.Warn(WarnDeadBranch, tree, n.Pipe, "with branch is unreachable: condition is always false")
		}
	}
	withScope := child.child()
	if path := pipeFieldPath(n.Pipe); path != "" {
		withScope.guarded[path] = struct{}{}
	}
	// Inside {{with}}, dot is reassigned to the pipe value, which is known non-nil.
	withScope.guarded["."] = struct{}{}
	if _, err := withScope.walk(tree, x, nil, n.List); err != nil {
		return err
	}
	withScope.warnUnused(tree)
	if n.ElseList != nil {
		elseScope := child.child()
		if _, err := elseScope.walk(tree, dot, nil, n.ElseList); err != nil {
			return err
		}
		elseScope.warnUnused(tree)
	}
	return nil
}

func newNumberNodeType(tree *parse.Tree, constant *parse.NumberNode) (types.Type, error) {
	switch {
	case constant.IsComplex:
		return types.Typ[types.UntypedComplex], nil

	case constant.IsFloat &&
		!isHexInt(constant.Text) && !isRuneInt(constant.Text) &&
		strings.ContainsAny(constant.Text, ".eEpP"):
		return types.Typ[types.UntypedFloat], nil

	case constant.IsInt:
		n := int(constant.Int64)
		if int64(n) != constant.Int64 {
			return nil, newError(tree, constant, "%s overflows int", constant.Text)
		}
		return types.Typ[types.UntypedInt], nil

	case constant.IsUint:
		return nil, newError(tree, constant, "%s overflows int", constant.Text)
	}
	return types.Typ[types.UntypedInt], nil
}

func isRuneInt(s string) bool {
	return len(s) > 0 && s[0] == '\''
}

func isHexInt(s string) bool {
	return len(s) > 2 && s[0] == '0' && (s[1] == 'x' || s[1] == 'X') && !strings.ContainsAny(s, "pP")
}

func (s *scope) checkTemplateNode(tree *parse.Tree, dot types.Type, n *parse.TemplateNode) error {
	x := dot
	if n.Pipe != nil {
		tp, err := s.walk(tree, x, nil, n.Pipe)
		if err != nil {
			return err
		}
		x = tp
		x = downgradeUntyped(x)
	} else {
		x = types.Typ[types.UntypedNil]
	}
	if fn := s.global.InspectTemplateNode; fn != nil {
		fn(n, tree, x)
	}
	// Record this call site's data type for post-walk W007 detection.
	if s.global.subTemplateCallTypes != nil {
		s.global.subTemplateCallTypes[n.Name] = append(s.global.subTemplateCallTypes[n.Name], x)
	}
	childTree, ok := s.global.trees.FindTree(n.Name)
	if !ok {
		return newError(tree, n, "template %q not found", n.Name)
	}
	childGuarded := make(map[string]struct{})
	// If dot is known non-nil in the current scope (e.g. inside a {{with}}
	// block), propagate that to the child template since the same value
	// is being passed as the child's dot.
	if _, dotGuarded := s.guarded["."]; dotGuarded {
		childGuarded["."] = struct{}{}
	}
	childScope := scope{
		global: s.global,
		variables: map[string]types.Type{
			"$": x,
		},
		guarded:   childGuarded,
		nonilVars: make(map[string]struct{}),
		declared:  make(map[string]parse.Node),
		used:      make(map[string]struct{}),
	}
	_, err := childScope.walk(childTree, x, nil, childTree.Root)
	childScope.warnUnused(childTree)
	return err
}

func downgradeUntyped(x types.Type) types.Type {
	if x == nil {
		return x
	}
	basic, ok := x.Underlying().(*types.Basic)
	if !ok {
		return x
	}
	switch k := basic.Kind(); k {
	case types.UntypedInt:
		return types.Typ[types.Int].Underlying()
	case types.UntypedRune:
		return types.Typ[types.Rune].Underlying()
	case types.UntypedFloat:
		return types.Typ[types.Float64].Underlying()
	case types.UntypedComplex:
		return types.Typ[types.Complex128].Underlying()
	case types.UntypedString:
		return types.Typ[types.String].Underlying()
	default:
		return x
	}
}

func (s *scope) checkFieldNode(tree *parse.Tree, dot types.Type, n *parse.FieldNode, args []types.Type) (types.Type, error) {
	return s.checkIdentifiers(tree, dot, n, n.Ident, args)
}

func (s *scope) checkCommandNode(tree *parse.Tree, dot, prev types.Type, cmd *parse.CommandNode) (types.Type, error) {
	first := cmd.Args[0]
	switch n := first.(type) {
	case *parse.FieldNode:
		argTypes, err := s.argumentTypes(tree, dot, prev, cmd.Args[1:])
		if err != nil {
			return nil, err
		}
		return s.checkFieldNode(tree, dot, n, argTypes)
	case *parse.ChainNode:
		argTypes, err := s.argumentTypes(tree, dot, prev, cmd.Args[1:])
		if err != nil {
			return nil, err
		}
		return s.checkChainNode(tree, dot, prev, n, argTypes)
	case *parse.IdentifierNode:
		var argTypes []types.Type
		var err error
		if n.Ident == "and" {
			argTypes, err = s.argumentTypesAnd(tree, dot, prev, cmd.Args[1:])
		} else {
			argTypes, err = s.argumentTypes(tree, dot, prev, cmd.Args[1:])
		}
		if err != nil {
			return nil, err
		}
		tp, err := s.global.calls.CheckCall(s.global, n.Ident, cmd.Args[1:], argTypes)
		if err != nil {
			return nil, wrapError(tree, cmd, err)
		}
		return tp, nil
	case *parse.PipeNode:
		if err := s.notAFunction(tree, n, cmd.Args, prev); err != nil {
			return nil, err
		}
		return s.checkPipeNode(tree, dot, n)
	case *parse.VariableNode:
		argTypes, err := s.argumentTypes(tree, dot, prev, cmd.Args[1:])
		if err != nil {
			return nil, err
		}
		return s.checkVariableNode(tree, n, argTypes)
	}

	if err := s.notAFunction(tree, first, cmd.Args, prev); err != nil {
		return nil, err
	}

	switch n := first.(type) {
	case *parse.BoolNode:
		return types.Typ[types.UntypedBool], nil
	case *parse.StringNode:
		return types.Typ[types.UntypedString], nil
	case *parse.NumberNode:
		return newNumberNodeType(tree, n)
	case *parse.DotNode:
		return dot, nil
	case *parse.NilNode:
		return nil, newError(tree, n, "nil is not a command")
	default:
		return nil, newError(tree, first, "can't evaluate command %q", first)
	}
}

// argumentTypesAnd evaluates arguments to the built-in "and" function
// left-to-right, propagating nil-safety from earlier arguments to later
// ones. If argument N is a bare variable or field reference to a pointer
// type, arguments N+1..last are evaluated with that path guarded,
// because Go's template "and" short-circuits on the first falsy value.
func (s *scope) argumentTypesAnd(tree *parse.Tree, dot types.Type, prev types.Type, args []parse.Node) ([]types.Type, error) {
	argTypes := make([]types.Type, 0, len(args)+1)
	var addedGuards []string
	var addedNonilVars []string
	defer func() {
		for _, g := range addedGuards {
			delete(s.guarded, g)
		}
		for _, v := range addedNonilVars {
			delete(s.nonilVars, v)
		}
	}()
	for _, arg := range args {
		argType, err := s.walk(tree, dot, prev, arg)
		if err != nil {
			return nil, err
		}
		argTypes = append(argTypes, argType)
		// If this argument is a bare variable/field reference to a pointer
		// type, guard it for subsequent arguments.
		if path := nodeGuardPath(arg); path != "" {
			if strings.HasPrefix(path, "$") && !strings.Contains(path, ".") {
				// Bare variable like "$u" — mark as nonil.
				if _, already := s.nonilVars[path]; !already {
					s.nonilVars[path] = struct{}{}
					addedNonilVars = append(addedNonilVars, path)
				}
			} else if _, already := s.guarded[path]; !already {
				s.guarded[path] = struct{}{}
				addedGuards = append(addedGuards, path)
			}
		}
	}
	if prev != nil {
		argTypes = append(argTypes, prev)
	}
	return argTypes, nil
}

// nodeGuardPath extracts a guardable path from a parse node, if the node
// is a bare variable or field reference. Returns "" if the node is not
// a simple reference that can serve as a nil guard.
func nodeGuardPath(node parse.Node) string {
	switch n := node.(type) {
	case *parse.FieldNode:
		return "." + strings.Join(n.Ident, ".")
	case *parse.VariableNode:
		if len(n.Ident) >= 2 && n.Ident[0] == "$" {
			// $.User → "$." prefix to distinguish from bare variable $u
			return "$." + strings.Join(n.Ident[1:], ".")
		}
		if len(n.Ident) == 1 {
			return n.Ident[0] // bare variable like "$u"
		}
	case *parse.CommandNode:
		if len(n.Args) == 1 {
			return nodeGuardPath(n.Args[0])
		}
	}
	return ""
}

func (s *scope) argumentTypes(tree *parse.Tree, dot types.Type, prev types.Type, args []parse.Node) ([]types.Type, error) {
	argTypes := make([]types.Type, 0, len(args)+1)
	for _, arg := range args {
		argType, err := s.walk(tree, dot, prev, arg)
		if err != nil {
			return nil, err
		}
		argTypes = append(argTypes, argType)
	}
	if prev != nil {
		argTypes = append(argTypes, prev)
	}
	return argTypes, nil
}

func (s *scope) notAFunction(tree *parse.Tree, node parse.Node, args []parse.Node, final types.Type) error {
	if len(args) > 1 || final != nil {
		return newError(tree, node, "can't give argument to non-function %s", args[0])
	}
	return nil
}

func (s *scope) checkIdentifiers(tree *parse.Tree, dot types.Type, n parse.Node, idents []string, args []types.Type) (types.Type, error) {
	x := dot
	// prevType tracks the type before field resolution so we can check
	// whether a pointer-typed field has a templatecheck:"nonil" tag.
	var prevType types.Type
	for i, ident := range idents {
		if _, isPtr := x.(*types.Pointer); isPtr && s.global.Warn != nil {
			// Build the path of the pointer value being dereferenced.
			// If i==0, the pointer is dot itself (path ".").
			// If i>0, it's the field path up to this point (e.g. ".Bar").
			var ptrPath string
			if i == 0 {
				ptrPath = "."
			} else {
				ptrPath = "." + strings.Join(idents[:i], ".")
			}
			// Suppress W003 if the field that produced this pointer has
			// a `templatecheck:"nonil"` struct tag.
			if i > 0 && s.global.isNonilField(dereference(prevType), idents[i-1]) {
				// nonil-tagged field — skip warning
			} else if _, guarded := s.guarded[ptrPath]; !guarded {
				s.global.Warn(WarnNilDereference, tree, n, fmt.Sprintf("accessing .%s on pointer type %s may panic if nil; consider guarding with {{if}} or {{with}}, or add a `templatecheck:\"nonil\"` struct tag", ident, s.global.TypeString(x)))
			}
		}
		prevType = x
		x = dereference(x)
		switch xx := x.(type) {
		case *types.Map:
			switch key := xx.Key().Underlying().(type) {
			case *types.Basic:
				switch key.Kind() {
				// case types.Int, types.Int64, types.Int32, types.Int16, types.Int8,
				//	types.Uint, types.Uint64, types.Uint32, types.Uint16, types.Uint8:
				case types.Int:
					x = xx.Elem()
					_, err := strconv.Atoi(ident)
					if err != nil {
						return nil, newError(tree, n, `can't evaluate field one in type %s`, s.global.TypeString(xx))
					}
				case types.String:
					x = xx.Elem()
				default:
				}
				continue
			default:
				x = xx.Elem()
			}
			continue
		default:
			if !token.IsExported(ident) {
				return nil, newError(tree, n, "field or method %s is not exported", ident)
			}
			obj, _, _ := types.LookupFieldOrMethod(x, true, s.global.pkg, ident)
			if obj == nil {
				if types.IsInterface(x) {
					if s.global.Warn != nil {
						s.global.Warn(WarnInterfaceFieldAccess, tree, n, fmt.Sprintf("field access .%s on interface type %s cannot be statically verified", ident, s.global.TypeString(x)))
					}
					x = types.NewInterfaceType(nil, nil)
					continue
				}
				return nil, newError(tree, n, "%s not found on %s", ident, s.global.TypeString(x))
			}
			switch o := obj.(type) {
			default:
				x = obj.Type()
				// If the field has a templatecheck:"nonil" struct tag,
				// set resultNonil so it propagates through variable
				// assignment (e.g. $s := .S where S is nonil).
				if _, isPtr := x.(*types.Pointer); isPtr {
					if s.global.isNonilField(xx, ident) {
						s.resultNonil = true
					}
				}
			case *types.Func:
				sig := o.Signature()
				resultLen := sig.Results().Len()
				if resultLen < 1 || resultLen > 2 {
					methodPos := s.global.fileSet.Position(o.Pos())
					return nil, newError(tree, n, "function %s has %d return values; should be 1 or 2: incorrect signature at %s", ident, resultLen, methodPos)
				}
				if resultLen > 1 {
					methodPos := s.global.fileSet.Position(obj.Pos())
					finalResult := sig.Results().At(sig.Results().Len() - 1)
					errorType := types.Universe.Lookup("error")
					if !types.Identical(errorType.Type(), finalResult.Type()) {
						return nil, newError(tree, n, "invalid function signature for %s: second return value should be error; is %s: incorrect signature at %s", ident, s.global.TypeString(finalResult.Type()), methodPos)
					}
				}
				if i == len(idents)-1 {
					res, err := checkCallArguments(s.global, sig, args)
					if err != nil {
						return nil, wrapError(tree, n, err)
					}
					return res, nil
				}
				x = sig.Results().At(0).Type()
			}
			if _, ok := x.(*types.Signature); ok && i < len(idents)-1 {
				return nil, newError(tree, n, "identifier chain not supported for type %s", s.global.TypeString(x))
			}
		}
	}
	if len(args) > 0 {
		sig, ok := x.(*types.Signature)
		if !ok {
			return nil, newError(tree, n, "expected method or function")
		}
		tp, err := checkCallArguments(s.global, sig, args)
		if err != nil {
			return nil, wrapError(tree, n, err)
		}
		return tp, nil
	}
	return x, nil
}

func (s *scope) checkRangeNode(tree *parse.Tree, dot types.Type, n *parse.RangeNode) error {
	child := s.child()
	pipeType, err := child.walk(tree, dot, nil, n.Pipe)
	if err != nil {
		return err
	}
	// Range iteration variables are structural — don't warn if unused.
	for _, decl := range n.Pipe.Decl {
		if len(decl.Ident) > 0 {
			delete(child.declared, decl.Ident[0])
		}
	}
	pipeType = dereference(pipeType).Underlying()
	var x types.Type
	switch pt := pipeType.(type) {
	case *types.Slice:
		x = pt.Elem()
		if len(n.Pipe.Decl) == 1 {
			child.variables[n.Pipe.Decl[0].Ident[0]] = x
		} else if len(n.Pipe.Decl) > 1 {
			child.variables[n.Pipe.Decl[0].Ident[0]] = types.Typ[types.Int]
			child.variables[n.Pipe.Decl[1].Ident[0]] = x
		}
	case *types.Array:
		x = pt.Elem()
		if len(n.Pipe.Decl) == 1 {
			child.variables[n.Pipe.Decl[0].Ident[0]] = x
		} else if len(n.Pipe.Decl) > 1 {
			child.variables[n.Pipe.Decl[0].Ident[0]] = types.Typ[types.Int]
			child.variables[n.Pipe.Decl[1].Ident[0]] = x
		}
	case *types.Map:
		x = pt.Elem()
		if len(n.Pipe.Decl) == 1 {
			child.variables[n.Pipe.Decl[0].Ident[0]] = pt.Elem()
		} else if len(n.Pipe.Decl) > 1 {
			child.variables[n.Pipe.Decl[0].Ident[0]] = pt.Key()
			child.variables[n.Pipe.Decl[1].Ident[0]] = pt.Elem()
		}
	case *types.Chan:
		x = pt.Elem()
		if len(n.Pipe.Decl) > 1 {
			child.variables[n.Pipe.Decl[0].Ident[0]] = types.Typ[types.Int] // TODO: this looks odd, I don't think I should permit an index here
			child.variables[n.Pipe.Decl[1].Ident[0]] = pt.Elem()
		}
	case *types.Basic:
		switch {
		case pt.Info()&types.IsInteger != 0:
			tp := types.Universe.Lookup(strings.TrimPrefix(pipeType.String(), "untyped "))
			x = tp.Type()
			if len(n.Pipe.Decl) > 0 {
				child.variables[n.Pipe.Decl[0].Ident[0]] = x
			}
			return nil
		default:
			return newError(tree, n.Pipe, "range can't iterate over %s", strings.TrimPrefix(s.global.TypeString(pipeType), "untyped "))
		}
	case *types.Signature:
		if v1, v2, ok := isIter2(pt); ok {
			x = v1
			if len(n.Pipe.Decl) > 0 {
				child.variables[n.Pipe.Decl[0].Ident[0]] = v1
			}
			if len(n.Pipe.Decl) > 1 {
				child.variables[n.Pipe.Decl[1].Ident[0]] = v2
			}
			return nil
		}
		if val, ok := isIter(pt); ok {
			x = val
			if len(n.Pipe.Decl) == 1 {
				child.variables[n.Pipe.Decl[0].Ident[0]] = val
			}
			if len(n.Pipe.Decl) > 1 {
				return newError(tree, n.Pipe, "iter.Seq[T] must not iterate over more than one variable")
			}
			return nil
		}
		return newError(tree, n.Pipe, "failed to range over function %s", s.global.TypeString(pipeType))
	default:
		return newError(tree, n.Pipe, "failed to range over %s", s.global.TypeString(pipeType))
	}
	if _, err := child.walk(tree, x, nil, n.List); err != nil {
		return err
	}
	child.warnUnused(tree)
	if n.ElseList != nil {
		elseScope := s.child()
		if _, err := elseScope.walk(tree, x, nil, n.ElseList); err != nil {
			return err
		}
		elseScope.warnUnused(tree)
	}
	return nil
}

func isIter(signature *types.Signature) (types.Type, bool) {
	if signature == nil || signature.Variadic() || signature.Results().Len() != 0 || signature.Params().Len() != 1 {
		return nil, false
	}
	yield, ok := signature.Params().At(0).Type().(*types.Signature)
	if !ok || yield.Results().Len() != 1 || yield.Params().Len() != 1 {
		return nil, false
	}
	if !types.Identical(yield.Results().At(0).Type(), types.Universe.Lookup("bool").Type()) {
		return nil, false
	}
	return yield.Params().At(0).Type(), true
}

func isIter2(signature *types.Signature) (types.Type, types.Type, bool) {
	if signature == nil || signature.Variadic() || signature.Results().Len() != 0 || signature.Params().Len() != 1 {
		return nil, nil, false
	}
	yield, ok := signature.Params().At(0).Type().(*types.Signature)
	if !ok || yield.Results().Len() != 1 || yield.Params().Len() != 2 {
		return nil, nil, false
	}
	if !types.Identical(yield.Results().At(0).Type(), types.Universe.Lookup("bool").Type()) {
		return nil, nil, false
	}
	yp := yield.Params()
	return yp.At(0).Type(), yp.At(1).Type(), true
}

func (s *scope) checkIdentifierNode(tree *parse.Tree, n *parse.IdentifierNode) (types.Type, error) {
	if !strings.HasPrefix(n.Ident, "$") {
		tp, err := s.global.calls.CheckCall(s.global, n.Ident, nil, nil)
		if err != nil {
			return nil, wrapError(tree, n, err)
		}
		return tp, err
	}
	tp, ok := s.variables[n.Ident]
	if !ok {
		return nil, newError(tree, n, "failed to find identifier %s", n.Ident)
	}
	return tp, nil
}

func dereference(tp types.Type) types.Type {
	for {
		ptr, ok := tp.(*types.Pointer)
		if !ok {
			return tp
		}
		tp = ptr.Elem()
	}
}

// isNonilField reports whether the given field on the given type has a
// `templatecheck:"nonil"` struct tag. Results are cached on the Global.
func (g *Global) isNonilField(typ types.Type, fieldName string) bool {
	if g.nonilFields == nil {
		g.nonilFields = make(map[*types.Struct]map[string]struct{})
	}
	// Use LookupFieldOrMethod to find the field, which handles embedded
	// structs (e.g. S on a struct that embeds PageData).
	obj, _, _ := types.LookupFieldOrMethod(typ, true, nil, fieldName)
	if obj == nil {
		return false
	}
	v, ok := obj.(*types.Var)
	if !ok || !v.IsField() {
		return false
	}
	// Find the struct that directly declares this field.
	parent := v.Parent()
	_ = parent
	// Walk the type to find which struct contains the field with its tag.
	return g.fieldHasNonilTag(typ, fieldName)
}

// fieldHasNonilTag checks direct and embedded struct fields for the nonil tag.
func (g *Global) fieldHasNonilTag(typ types.Type, fieldName string) bool {
	st, ok := typ.Underlying().(*types.Struct)
	if !ok {
		return false
	}
	if fields, cached := g.nonilFields[st]; cached {
		_, nonil := fields[fieldName]
		return nonil
	}
	// Scan ALL direct fields for nonil tags and recurse into embedded structs.
	// Cache every tagged field so subsequent lookups for different field names
	// on the same struct are cache hits.
	fields := make(map[string]struct{})
	for i := 0; i < st.NumFields(); i++ {
		f := st.Field(i)
		if strings.Contains(st.Tag(i), `templatecheck:"nonil"`) {
			fields[f.Name()] = struct{}{}
		}
		// Recurse into embedded (anonymous) structs to find inherited tags.
		if f.Anonymous() {
			embeddedType := f.Type()
			if ptr, ok := embeddedType.(*types.Pointer); ok {
				embeddedType = ptr.Elem()
			}
			if embSt, ok := embeddedType.Underlying().(*types.Struct); ok {
				// Ensure embedded struct is scanned first.
				g.fieldHasNonilTag(embeddedType, "")
				if cached, ok := g.nonilFields[embSt]; ok {
					for name := range cached {
						fields[name] = struct{}{}
					}
				}
			}
		}
	}
	g.nonilFields[st] = fields
	_, nonil := fields[fieldName]
	return nonil
}
