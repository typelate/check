package check

import (
	"bytes"
	"errors"
	"fmt"
	"go/token"
	"go/types"
	"maps"
	"strconv"
	"strings"
	"text/template/parse"
)

// ErrorType classifies the failure a *Error reports. It lets tools such as
// language servers map errors to stable diagnostic codes without parsing
// error messages.
type ErrorType int

const (
	// ErrorTypeUnknown marks errors not classified by this package, such as
	// those returned by a custom CallChecker implementation.
	ErrorTypeUnknown ErrorType = iota
	// ErrorTypeAggregate marks an error that only groups child errors.
	ErrorTypeAggregate
	// ErrorTypeUnexpectedNode reports a parse node the checker does not know
	// how to walk.
	ErrorTypeUnexpectedNode
	// ErrorTypeVariableNotFound reports use of an undeclared template variable.
	ErrorTypeVariableNotFound
	// ErrorTypeConstantOverflow reports a numeric constant that overflows int.
	ErrorTypeConstantOverflow
	// ErrorTypeTemplateNotFound reports invoking an undefined associated template.
	ErrorTypeTemplateNotFound
	// ErrorTypeBadCommand reports a command that cannot be evaluated, such as nil.
	ErrorTypeBadCommand
	// ErrorTypeNotAFunction reports arguments given to a non-function value.
	ErrorTypeNotAFunction
	// ErrorTypeFieldNotExported reports access to an unexported field or method.
	ErrorTypeFieldNotExported
	// ErrorTypeFieldOrMethodNotFound reports a field or method lookup failure.
	ErrorTypeFieldOrMethodNotFound
	// ErrorTypeBadSignature reports a method or function whose signature is
	// not callable from a template.
	ErrorTypeBadSignature
	// ErrorTypeCallArguments reports a call with the wrong number or types of
	// arguments.
	ErrorTypeCallArguments
	// ErrorTypeUnknownFunction reports a call to an undefined function.
	ErrorTypeUnknownFunction
	// ErrorTypeRange reports a range over a type that cannot be iterated.
	ErrorTypeRange
	// ErrorTypeIdentifierChain reports a field chain through a type that does
	// not support further selection.
	ErrorTypeIdentifierChain
	// ErrorTypeMapKey reports a map index whose key cannot match the map's
	// key type.
	ErrorTypeMapKey
)

// String returns a stable slug for the error type, suitable for use as a
// diagnostic code.
func (t ErrorType) String() string {
	switch t {
	case ErrorTypeAggregate:
		return "aggregate"
	case ErrorTypeUnexpectedNode:
		return "unexpected-node"
	case ErrorTypeVariableNotFound:
		return "variable-not-found"
	case ErrorTypeConstantOverflow:
		return "constant-overflow"
	case ErrorTypeTemplateNotFound:
		return "template-not-found"
	case ErrorTypeBadCommand:
		return "bad-command"
	case ErrorTypeNotAFunction:
		return "not-a-function"
	case ErrorTypeFieldNotExported:
		return "field-not-exported"
	case ErrorTypeFieldOrMethodNotFound:
		return "field-or-method-not-found"
	case ErrorTypeBadSignature:
		return "bad-signature"
	case ErrorTypeCallArguments:
		return "call-arguments"
	case ErrorTypeUnknownFunction:
		return "unknown-function"
	case ErrorTypeRange:
		return "range"
	case ErrorTypeIdentifierChain:
		return "identifier-chain"
	case ErrorTypeMapKey:
		return "map-key"
	default:
		return "unknown"
	}
}

type Error struct {
	// Type classifies the failure.
	Type ErrorType

	Tree *parse.Tree
	Node parse.Node

	// X is the type most relevant to the failure: the receiver for field or
	// method lookups, the pipeline result for range, the signature for call
	// errors. It is nil when no type is relevant.
	X types.Type

	// Decl is the source position where the Go declaration involved in the
	// failure is defined: the receiver type for field or method lookups,
	// the method for signature failures. It is the zero value when no
	// declaration position is known.
	Decl token.Position

	// Secondary marks a follow-on failure whose root cause is another error
	// in the same tree: a variable lookup that failed only because the
	// pipeline declaring that variable already failed. Diagnostic tools may
	// suppress or de-emphasize secondary errors.
	Secondary bool

	err error

	// render re-renders the cause message with a caller-chosen type
	// formatter. It is set when the message embeds type names; nil means
	// the message has no types to re-render.
	render func(typeFormatFunc) string

	// children holds the child errors when this error aggregates several
	// independent failures found while walking the same subtree.
	children []*Error
}

func newError(errType ErrorType, tree *parse.Tree, node parse.Node, message string, args ...any) *Error {
	e := errorf(errType, message, args...)
	e.Tree = tree
	e.Node = node
	return e
}

// errorf builds a located-later *Error: the walk site that receives it fills
// in Tree and Node via wrapError. types.Type args are rendered with a nil
// qualifier (full package paths) in the Error message and re-rendered with
// the caller's qualifier by DetailedError.
func errorf(errType ErrorType, message string, args ...any) *Error {
	render := renderer(message, args...)
	return &Error{
		Type:   errType,
		err:    errors.New(render(fullTypeFormat(nil))),
		render: render,
	}
}

// withX sets the type most relevant to the failure and returns e.
func (e *Error) withX(x types.Type) *Error {
	e.X = x
	return e
}

// withDecl sets the position of the involved Go declaration and returns e.
func (e *Error) withDecl(pos token.Position) *Error {
	e.Decl = pos
	return e
}

// wrapError locates err at node. When err is already a *Error, its missing
// location and classification are filled in on a copy; otherwise err becomes
// the cause of a new leaf error.
func wrapError(errType ErrorType, tree *parse.Tree, node parse.Node, err error) *Error {
	if e, ok := err.(*Error); ok {
		located := *e
		if located.Tree == nil {
			located.Tree = tree
		}
		if located.Node == nil {
			located.Node = node
		}
		if located.Type == ErrorTypeUnknown {
			located.Type = errType
		}
		return &located
	}
	return &Error{
		Type: errType,
		Tree: tree,
		Node: node,
		err:  err,
	}
}

// Error returns the single-line error message. The format is
//
//	{file}:{line}:{col}: executing {tree-name} at <{node-text}>: {message}
//
// The leading file:line:col is recognized by terminals and IDEs as a
// jump-to-source location. The message after the location matches the
// shape produced by text/template at runtime.
func (e *Error) Error() string {
	if len(e.children) > 0 {
		messages := make([]string, len(e.children))
		for i, child := range e.children {
			messages[i] = child.Error()
		}
		return strings.Join(messages, "\n")
	}
	return e.line(fullTypeFormat(nil))
}

// typeFormatFunc renders a types.Type to a string. Error uses
// fullTypeFormat(nil); DetailedError uses a qualifying, eliding formatter.
type typeFormatFunc func(types.Type) string

// fullTypeFormat renders complete type strings qualified with q.
func fullTypeFormat(q types.Qualifier) typeFormatFunc {
	return func(tp types.Type) string {
		return formatType(tp, q)
	}
}

// line renders the single-line message for a leaf error, rendering type
// names with tf.
func (e *Error) line(tf typeFormatFunc) string {
	message := e.messageWith(tf)
	if e.Tree == nil || e.Node == nil {
		return message
	}
	loc, ctx := e.Tree.ErrorContext(e.Node)
	return fmt.Sprintf("%s: executing %q at <%s>: %s", loc, e.Tree.Name, ctx, message)
}

// messageWith renders the cause message through tf when the error (or its
// cause) recorded how to re-render its message.
func (e *Error) messageWith(tf typeFormatFunc) string {
	if e.render != nil {
		return e.render(tf)
	}
	if identErr, ok := errors.AsType[*IdentifierError](e.err); ok && identErr.render != nil {
		return identErr.render(tf)
	}
	if callErr, ok := errors.AsType[*CallError](e.err); ok && callErr.render != nil {
		return callErr.render(tf)
	}
	return e.err.Error()
}

// renderFormat is fmt.Sprintf with types.Type args rendered through tf.
func renderFormat(tf typeFormatFunc, format string, args ...any) string {
	rendered := make([]any, len(args))
	for i, arg := range args {
		if tp, ok := arg.(types.Type); ok {
			rendered[i] = tf(tp)
			continue
		}
		rendered[i] = arg
	}
	return fmt.Sprintf(format, rendered...)
}

// renderer captures format and args for re-rendering with any type
// formatter.
func renderer(format string, args ...any) func(typeFormatFunc) string {
	return func(tf typeFormatFunc) string {
		return renderFormat(tf, format, args...)
	}
}

// Unwrap exposes the underlying errors: the wrapped cause for a leaf error,
// or the child errors for an aggregate.
func (e *Error) Unwrap() []error {
	errs := make([]error, 0, len(e.children)+1)
	if e.err != nil {
		errs = append(errs, e.err)
	}
	for _, child := range e.children {
		errs = append(errs, child)
	}
	return errs
}

// All is an iter.Seq[*Error] that walks the error tree in depth-first
// pre-order, yielding e itself first and then the subtree of each child:
//
//	for err := range checkErr.All { ... }
func (e *Error) All(yield func(*Error) bool) {
	e.walkAll(yield)
}

func (e *Error) walkAll(yield func(*Error) bool) bool {
	if !yield(e) {
		return false
	}
	for _, child := range e.children {
		if !child.walkAll(yield) {
			return false
		}
	}
	return true
}

// joinErrors combines the non-nil errors found while walking node into a
// single error: nil when there are none, the error itself when there is one,
// and an aggregate *Error otherwise. Errors that are not *Error are promoted
// to leaf errors located at node.
func joinErrors(tree *parse.Tree, node parse.Node, errs ...error) error {
	var children []*Error
	for _, err := range errs {
		if err == nil {
			continue
		}
		child, ok := err.(*Error)
		if !ok {
			child = wrapError(ErrorTypeUnknown, tree, node, err)
		}
		children = append(children, child)
	}
	switch len(children) {
	case 0:
		return nil
	case 1:
		return children[0]
	default:
		return &Error{Type: ErrorTypeAggregate, Tree: tree, Node: node, children: children}
	}
}

// VerboseError returns a (possibly) multi-line message. The single-line
// summary returned by Error stays on the first line; if the wrapped error
// implements VerboseErrorer and contributes additional detail, that detail
// is indented on subsequent lines.
func (e *Error) VerboseError() string {
	if len(e.children) > 0 {
		blocks := make([]string, len(e.children))
		for i, child := range e.children {
			blocks[i] = child.VerboseError()
		}
		return strings.Join(blocks, "\n\n")
	}
	var prefix string
	if e.Tree != nil && e.Node != nil {
		loc, ctx := e.Tree.ErrorContext(e.Node)
		prefix = fmt.Sprintf("%s: executing %q at <%s>: ", loc, e.Tree.Name, ctx)
	}
	v, ok := errors.AsType[VerboseErrorer](e.err)
	if !ok {
		return prefix + e.err.Error()
	}
	verbose := v.VerboseError()
	first, rest, hasRest := strings.Cut(verbose, "\n")
	if !hasRest {
		return prefix + first
	}
	indented := strings.ReplaceAll(rest, "\n", "\n  ")
	return prefix + first + "\n  " + indented
}

type Global struct {
	trees TreeFinder
	calls CallChecker

	pkg             *types.Package
	fileSet         *token.FileSet
	typeNodeMapping TypeNodeMapping

	InspectTemplateNode TemplateNodeInspectorFunc
	InspectCallNode     ExecuteTemplateNodeInspectorFunc

	// Qualifier controls how types are printed by TypeString and by the
	// legacy VerboseError/FormatVerbose rendering only. Error messages
	// always print full package paths (nil qualifier), and DetailedError
	// takes its own types.Qualifier parameter — prefer that for qualified
	// rendering. See types.WriteType for qualifier semantics.
	Qualifier types.Qualifier
}

type TemplateNodeInspectorFunc func(node *parse.TemplateNode, t *parse.Tree, tp types.Type)

func NewGlobal(pkg *types.Package, fileSet *token.FileSet, trees TreeFinder, fnChecker CallChecker) *Global {
	return &Global{
		trees:           trees,
		calls:           fnChecker,
		pkg:             pkg,
		fileSet:         fileSet,
		typeNodeMapping: make(TypeNodeMapping),
	}
}

// TypeString returns the string representation of typ using the configured Qualifier.
func (g *Global) TypeString(typ types.Type) string {
	var buf bytes.Buffer
	types.WriteType(&buf, typ, g.Qualifier)
	return buf.String()
}

// notFoundMessage returns a renderer for the compact single-line not-found
// message. The exported members of tp are not listed here; DetailedError
// renders them.
func notFoundMessage(ident string, tp types.Type, fset *token.FileSet) func(typeFormatFunc) string {
	return func(tf typeFormatFunc) string {
		var b strings.Builder
		b.WriteString("field or method ")
		b.WriteString(ident)
		b.WriteString(" not found on ")
		b.WriteString(tf(tp))
		if named, ok := tp.(*types.Named); ok {
			pos := fset.Position(named.Obj().Pos())
			if pos.IsValid() {
				fmt.Fprintf(&b, " (declared at %s)", pos)
			}
		}
		return b.String()
	}
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

// Execute type-checks tree against data, the type of the template's root
// context (dot). It returns nil when the template checks cleanly. Otherwise
// the returned error is a *Error: a single leaf for one failure, or an
// ErrorTypeAggregate node grouping every independent failure found. Walk the
// full tree with Error.All or Unwrap; each leaf carries the ErrorType
// classification, the parse.Tree and parse.Node it was found at, and, when
// relevant, the types.Type being checked.
func Execute(global *Global, tree *parse.Tree, data types.Type) error {
	s := &scope{
		global: global,
		variables: map[string]types.Type{
			"$": data,
		},
	}
	_, err := s.walk(tree, data, nil, tree.Root)
	return err
}

type scope struct {
	global    *Global
	variables map[string]types.Type

	// failed records variables whose declaration pipeline failed to check,
	// so later lookups can be marked Secondary instead of reported as
	// independent failures.
	failed map[string]bool
}

func (s *scope) child() *scope {
	return &scope{
		global:    s.global,
		variables: maps.Clone(s.variables),
		failed:    maps.Clone(s.failed),
	}
}

// markFailedDecls records the variables a failing pipeline would have
// declared.
func (s *scope) markFailedDecls(n *parse.PipeNode) {
	for _, decl := range n.Decl {
		if len(decl.Ident) == 0 {
			continue
		}
		if s.failed == nil {
			s.failed = make(map[string]bool)
		}
		s.failed[decl.Ident[0]] = true
	}
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
		return nil, newError(ErrorTypeUnexpectedNode, tree, n, "missing node type check %T", n)
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
	tp, ok := s.variables[n.Ident[0]]
	if !ok {
		e := newError(ErrorTypeVariableNotFound, tree, n, "variable %s not found", n.Ident[0])
		e.Secondary = s.failed[n.Ident[0]]
		return nil, e
	}
	return s.checkIdentifiers(tree, tp, n, n.Ident[1:], args)
}

func (s *scope) checkListNode(tree *parse.Tree, dot, prev types.Type, n *parse.ListNode) error {
	var errs []error
	for _, child := range n.Nodes {
		if _, err := s.walk(tree, dot, prev, child); err != nil {
			errs = append(errs, err)
		}
	}
	return joinErrors(tree, n, errs...)
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
			s.markFailedDecls(n)
			return nil, err
		}
		result = tp
	}
	if len(n.Decl) > 0 && len(n.Decl[0].Ident) > 0 {
		s.variables[n.Decl[0].Ident[0]] = result
	}
	return result, nil
}

func (s *scope) checkIfNode(tree *parse.Tree, dot types.Type, n *parse.IfNode) error {
	var errs []error
	if _, err := s.walk(tree, dot, nil, n.Pipe); err != nil {
		errs = append(errs, err)
	}
	ifScope := s.child()
	if _, err := ifScope.walk(tree, dot, nil, n.List); err != nil {
		errs = append(errs, err)
	}
	if n.ElseList != nil {
		elseScope := s.child()
		if _, err := elseScope.walk(tree, dot, nil, n.ElseList); err != nil {
			errs = append(errs, err)
		}
	}
	return joinErrors(tree, n, errs...)
}

func (s *scope) checkWithNode(tree *parse.Tree, dot types.Type, n *parse.WithNode) error {
	var errs []error
	child := s.child()
	x, err := child.walk(tree, dot, nil, n.Pipe)
	if err != nil {
		// The body's dot is unknown when the pipe fails, so only the
		// pipe and the else list (which keeps the outer dot) are checked.
		errs = append(errs, err)
	} else {
		withScope := child.child()
		if _, err := withScope.walk(tree, x, nil, n.List); err != nil {
			errs = append(errs, err)
		}
	}
	if n.ElseList != nil {
		elseScope := child.child()
		if _, err := elseScope.walk(tree, dot, nil, n.ElseList); err != nil {
			errs = append(errs, err)
		}
	}
	return joinErrors(tree, n, errs...)
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
			return nil, newError(ErrorTypeConstantOverflow, tree, constant, "%s overflows int", constant.Text)
		}
		return types.Typ[types.UntypedInt], nil

	case constant.IsUint:
		return nil, newError(ErrorTypeConstantOverflow, tree, constant, "%s overflows int", constant.Text)
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
	var errs []error
	pipeOK := true
	if n.Pipe != nil {
		tp, err := s.walk(tree, x, nil, n.Pipe)
		if err != nil {
			// The invoked template's dot is unknown when the pipe fails,
			// so only the pipe and the template lookup are checked.
			errs = append(errs, err)
			pipeOK = false
		} else {
			x = downgradeUntyped(tp)
		}
	} else {
		x = types.Typ[types.UntypedNil]
	}
	if fn := s.global.InspectTemplateNode; fn != nil && pipeOK {
		fn(n, tree, x)
	}
	childTree, ok := s.global.trees.FindTree(n.Name)
	if !ok {
		notFound := newError(ErrorTypeTemplateNotFound, tree, n, "template %q not found", n.Name)
		if pipeOK {
			notFound.X = x
		}
		errs = append(errs, notFound)
		return joinErrors(tree, n, errs...)
	}
	if pipeOK {
		childScope := scope{
			global: s.global,
			variables: map[string]types.Type{
				"$": x,
			},
		}
		if _, err := childScope.walk(childTree, x, nil, childTree.Root); err != nil {
			errs = append(errs, err)
		}
	}
	return joinErrors(tree, n, errs...)
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
		argTypes, err := s.argumentTypes(tree, dot, prev, cmd.Args[1:])
		if err != nil {
			return nil, err
		}
		tp, err := s.global.calls.CheckCall(s.global, n.Ident, cmd.Args[1:], argTypes)
		if err != nil {
			return nil, wrapError(ErrorTypeUnknown, tree, cmd, err)
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
		return nil, newError(ErrorTypeBadCommand, tree, n, "nil is not a command")
	default:
		return nil, newError(ErrorTypeBadCommand, tree, first, "can't evaluate command %q", first)
	}
}

func (s *scope) argumentTypes(tree *parse.Tree, dot types.Type, prev types.Type, args []parse.Node) ([]types.Type, error) {
	argTypes := make([]types.Type, 0, len(args)+1)
	var errs []error
	for _, arg := range args {
		argType, err := s.walk(tree, dot, prev, arg)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		argTypes = append(argTypes, argType)
	}
	if err := joinErrors(tree, nil, errs...); err != nil {
		return nil, err
	}
	if prev != nil {
		argTypes = append(argTypes, prev)
	}
	return argTypes, nil
}

func (s *scope) notAFunction(tree *parse.Tree, node parse.Node, args []parse.Node, final types.Type) error {
	if len(args) > 1 || final != nil {
		return newError(ErrorTypeNotAFunction, tree, node, "can't give argument to non-function %s", args[0])
	}
	return nil
}

// identErr builds an *Error wrapping an *IdentifierError. The location
// prefix is added by *Error at format time; the IdentifierError carries
// the full type so callers of FormatVerbose can render its structure.
// types.Type args are rendered with a nil qualifier in the Cause message
// and re-rendered with the caller's qualifier by DetailedError.
func (s *scope) identErr(errType ErrorType, tree *parse.Tree, n parse.Node, ident string, tp types.Type, format string, a ...any) *Error {
	render := renderer(format, a...)
	return wrapError(errType, tree, n, &IdentifierError{
		Identifier: ident,
		Type:       tp,
		Cause:      errors.New(render(fullTypeFormat(nil))),
		render:     render,
		qualifier:  s.global.Qualifier,
		fset:       s.global.fileSet,
	}).withX(tp)
}

func (s *scope) checkIdentifiers(tree *parse.Tree, dot types.Type, n parse.Node, idents []string, args []types.Type) (types.Type, error) {
	x := dot
	for i, ident := range idents {
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
						return nil, s.identErr(ErrorTypeMapKey, tree, n, ident, xx, `can't evaluate field one in type %s`, xx)
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
				return nil, s.identErr(ErrorTypeFieldNotExported, tree, n, ident, x, "field or method %s is not exported", ident)
			}
			obj, _, _ := types.LookupFieldOrMethod(x, true, s.global.pkg, ident)
			if obj == nil {
				render := notFoundMessage(ident, x, s.global.fileSet)
				notFound := wrapError(ErrorTypeFieldOrMethodNotFound, tree, n, &IdentifierError{
					Identifier: ident,
					Type:       x,
					Cause:      errors.New(render(fullTypeFormat(nil))),
					render:     render,
					qualifier:  s.global.Qualifier,
					fset:       s.global.fileSet,
				}).withX(x)
				if named, ok := x.(*types.Named); ok {
					notFound.Decl = s.global.fileSet.Position(named.Obj().Pos())
				}
				return nil, notFound
			}
			switch o := obj.(type) {
			default:
				x = obj.Type()
			case *types.Func:
				sig := o.Signature()
				resultLen := sig.Results().Len()
				if resultLen < 1 || resultLen > 2 {
					methodPos := s.global.fileSet.Position(o.Pos())
					return nil, s.identErr(ErrorTypeBadSignature, tree, n, ident, sig, "function %s has %d return values; should be 1 or 2: incorrect signature at %s", ident, resultLen, methodPos).withDecl(methodPos)
				}
				if resultLen > 1 {
					methodPos := s.global.fileSet.Position(obj.Pos())
					finalResult := sig.Results().At(sig.Results().Len() - 1)
					errorType := types.Universe.Lookup("error")
					if !types.Identical(errorType.Type(), finalResult.Type()) {
						return nil, s.identErr(ErrorTypeBadSignature, tree, n, ident, sig, "invalid function signature for %s: second return value should be error; is %s: incorrect signature at %s", ident, finalResult.Type(), methodPos).withDecl(methodPos)
					}
				}
				if i == len(idents)-1 {
					res, err := checkCallArguments(s.global, ident, sig, args)
					if err != nil {
						return nil, wrapError(ErrorTypeCallArguments, tree, n, err)
					}
					return res, nil
				}
				x = sig.Results().At(0).Type()
			}
			if _, ok := x.(*types.Signature); ok && i < len(idents)-1 {
				return nil, s.identErr(ErrorTypeIdentifierChain, tree, n, ident, x, "identifier chain not supported for type %s", x)
			}
		}
	}
	if len(args) > 0 {
		sig, ok := x.(*types.Signature)
		if !ok {
			return nil, s.identErr(ErrorTypeNotAFunction, tree, n, "", x, "expected method or function")
		}
		var name string
		if len(idents) > 0 {
			name = idents[len(idents)-1]
		}
		tp, err := checkCallArguments(s.global, name, sig, args)
		if err != nil {
			return nil, wrapError(ErrorTypeCallArguments, tree, n, err)
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
		default:
			return newError(ErrorTypeRange, tree, n.Pipe, "range can't iterate over %s", strings.TrimPrefix(s.global.TypeString(pipeType), "untyped ")).withX(pipeType)
		}
	case *types.Signature:
		if v1, v2, ok := isIter2(pt); ok {
			if len(n.Pipe.Decl) > 1 {
				x = v2
				child.variables[n.Pipe.Decl[0].Ident[0]] = v1
				child.variables[n.Pipe.Decl[1].Ident[0]] = v2
			} else {
				x = v1
				if len(n.Pipe.Decl) > 0 {
					child.variables[n.Pipe.Decl[0].Ident[0]] = v1
				}
			}
		} else if val, ok := isIter(pt); ok {
			x = val
			if len(n.Pipe.Decl) == 1 {
				child.variables[n.Pipe.Decl[0].Ident[0]] = val
			}
			if len(n.Pipe.Decl) > 1 {
				return newError(ErrorTypeRange, tree, n.Pipe, "iter.Seq[T] must not iterate over more than one variable").withX(pipeType)
			}
		} else {
			return newError(ErrorTypeRange, tree, n.Pipe, "failed to range over function %s", pipeType).withX(pipeType)
		}
	default:
		return newError(ErrorTypeRange, tree, n.Pipe, "failed to range over %s", pipeType).withX(pipeType)
	}
	var errs []error
	if _, err := child.walk(tree, x, nil, n.List); err != nil {
		errs = append(errs, err)
	}
	if n.ElseList != nil {
		if _, err := child.walk(tree, x, nil, n.ElseList); err != nil {
			errs = append(errs, err)
		}
	}
	return joinErrors(tree, n, errs...)
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
			return nil, wrapError(ErrorTypeUnknown, tree, n, err)
		}
		return tp, err
	}
	tp, ok := s.variables[n.Ident]
	if !ok {
		e := newError(ErrorTypeVariableNotFound, tree, n, "failed to find identifier %s", n.Ident)
		e.Secondary = s.failed[n.Ident]
		return nil, e
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
