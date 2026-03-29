package check

import (
	"fmt"
	"go/types"
	"maps"
	"text/template/parse"
)

type Functions map[string]*types.Signature

// DefaultFunctions returns the standard functions defined in html/template and text/template.
// It looks up escape functions (js, html, urlquery) from whichever template package is imported.
func DefaultFunctions(pkg *types.Package) Functions {
	fns := make(map[string]*types.Signature)
	for pn, idents := range map[string]map[string]string{
		"fmt": {
			"Sprint":   "print",
			"Sprintf":  "printf",
			"Sprintln": "println",
		},
		"html/template": {
			"JSEscaper":       "js",
			"URLQueryEscaper": "urlquery",
			"HTMLEscaper":     "html",
		},
		"text/template": {
			"JSEscaper":       "js",
			"URLQueryEscaper": "urlquery",
			"HTMLEscaper":     "html",
		},
	} {
		if p, ok := findPackage(pkg, pn); ok && p != nil {
			for funcIdent, templateFunc := range idents {
				obj := p.Scope().Lookup(funcIdent)
				if obj == nil {
					continue
				}
				sig, ok := obj.Type().(*types.Signature)
				if !ok {
					continue
				}
				fns[templateFunc] = sig
			}
		}
	}
	return fns
}

func (functions Functions) Add(m Functions) Functions {
	x := maps.Clone(functions)
	for name, sig := range m {
		x[name] = sig
	}
	return x
}

func (functions Functions) CheckCall(global *Global, funcIdent string, argNodes []parse.Node, argTypes []types.Type) (types.Type, error) {
	fn, ok := functions[funcIdent]
	if !ok {
		return builtInCheck(global, funcIdent, argNodes, argTypes)
	}
	if resultLen := fn.Results().Len(); resultLen == 0 {
		return nil, fmt.Errorf("function %s has no results", funcIdent)
	} else if resultLen > 2 {
		return nil, fmt.Errorf("function %s has too many results", funcIdent)
	}
	if funcIdent == "printf" {
		if err := checkPrintf(global, argNodes, argTypes); err != nil {
			return nil, err
		}
	}
	return checkCallArguments(global, fn, argTypes)
}

// checkPrintf validates that a printf format string's verbs match the
// argument types. It only checks when the format string is a static
// string node.
func checkPrintf(global *Global, argNodes []parse.Node, argTypes []types.Type) error {
	if len(argTypes) == 0 {
		return fmt.Errorf("printf requires a format string")
	}
	if len(argNodes) == 0 {
		// Format string was piped in — can't validate statically.
		return nil
	}
	fmtNode, ok := argNodes[0].(*parse.StringNode)
	if !ok {
		// Format string is not a static string — can't validate.
		return nil
	}
	format := fmtNode.Text
	verbs := parsePrintfVerbs(format)
	nArgs := len(argTypes) - 1 // first arg is the format string itself
	if len(verbs) != nArgs {
		return fmt.Errorf("printf format %q has %d verb(s) but %d argument(s)", format, len(verbs), nArgs)
	}
	for i, verb := range verbs {
		argType := argTypes[i+1]
		if err := checkPrintfVerb(global, verb, argType, i+1); err != nil {
			return err
		}
	}
	return nil
}

// printfVerb represents a single format verb extracted from a printf string.
type printfVerb struct {
	verb byte // the verb character (d, s, f, v, etc.)
}

// parsePrintfVerbs extracts format verbs from a printf format string.
func parsePrintfVerbs(format string) []printfVerb {
	var verbs []printfVerb
	for i := 0; i < len(format); i++ {
		if format[i] != '%' {
			continue
		}
		i++
		if i >= len(format) {
			break
		}
		// Skip '%%' (literal percent).
		if format[i] == '%' {
			continue
		}
		// Skip flags: #, 0, -, ' ', +
		for i < len(format) && (format[i] == '#' || format[i] == '0' || format[i] == '-' || format[i] == ' ' || format[i] == '+') {
			i++
		}
		// Skip width: digits or *
		for i < len(format) && ((format[i] >= '0' && format[i] <= '9') || format[i] == '*') {
			i++
		}
		// Skip precision: .digits or .*
		if i < len(format) && format[i] == '.' {
			i++
			for i < len(format) && ((format[i] >= '0' && format[i] <= '9') || format[i] == '*') {
				i++
			}
		}
		// The verb character.
		if i < len(format) {
			verbs = append(verbs, printfVerb{verb: format[i]})
		}
	}
	return verbs
}

// checkPrintfVerb validates that a single format verb is compatible with
// the argument type.
func checkPrintfVerb(global *Global, v printfVerb, argType types.Type, argIdx int) error {
	underlying := argType.Underlying()

	switch v.verb {
	case 'v', 'T':
		// %v and %T accept any type.
		return nil
	case 'd', 'b', 'o', 'O', 'x', 'X', 'c', 'U':
		// Integer verbs.
		if isIntegerType(underlying) {
			return nil
		}
		return fmt.Errorf("printf verb %%%c requires an integer, got %s for argument %d", v.verb, global.TypeString(argType), argIdx)
	case 'e', 'E', 'f', 'F', 'g', 'G':
		// Float verbs.
		if isFloatType(underlying) || isIntegerType(underlying) {
			return nil
		}
		return fmt.Errorf("printf verb %%%c requires a float, got %s for argument %d", v.verb, global.TypeString(argType), argIdx)
	case 's', 'q':
		// String verbs — accept string, []byte, error, Stringer, or any basic type.
		if isStringType(underlying) || isByteSlice(underlying) || implementsError(argType) || implementsStringer(argType) {
			return nil
		}
		return fmt.Errorf("printf verb %%%c requires a string, got %s for argument %d", v.verb, global.TypeString(argType), argIdx)
	case 'p':
		// Pointer verb.
		if _, ok := underlying.(*types.Pointer); ok {
			return nil
		}
		if _, ok := underlying.(*types.Slice); ok {
			return nil
		}
		if _, ok := underlying.(*types.Map); ok {
			return nil
		}
		if _, ok := underlying.(*types.Chan); ok {
			return nil
		}
		return fmt.Errorf("printf verb %%%c requires a pointer, got %s for argument %d", v.verb, global.TypeString(argType), argIdx)
	case 't':
		// Boolean verb.
		if b, ok := underlying.(*types.Basic); ok && b.Info()&types.IsBoolean != 0 {
			return nil
		}
		return fmt.Errorf("printf verb %%%c requires a bool, got %s for argument %d", v.verb, global.TypeString(argType), argIdx)
	case 'w':
		// %w for fmt.Errorf — requires error.
		if implementsError(argType) {
			return nil
		}
		return fmt.Errorf("printf verb %%%c requires an error, got %s for argument %d", v.verb, global.TypeString(argType), argIdx)
	default:
		// Unknown verb — don't error, could be a custom format.
		return nil
	}
}

func isIntegerType(t types.Type) bool {
	b, ok := t.(*types.Basic)
	return ok && b.Info()&types.IsInteger != 0
}

func isFloatType(t types.Type) bool {
	b, ok := t.(*types.Basic)
	return ok && (b.Info()&types.IsFloat != 0 || b.Info()&types.IsComplex != 0)
}

func isStringType(t types.Type) bool {
	b, ok := t.(*types.Basic)
	return ok && b.Info()&types.IsString != 0
}

func isByteSlice(t types.Type) bool {
	s, ok := t.(*types.Slice)
	if !ok {
		return false
	}
	b, ok := s.Elem().(*types.Basic)
	return ok && b.Kind() == types.Byte
}

func implementsError(t types.Type) bool {
	errorType := types.Universe.Lookup("error").Type().Underlying().(*types.Interface)
	return types.Implements(t, errorType) || types.Implements(types.NewPointer(t), errorType)
}

func implementsStringer(t types.Type) bool {
	// Check for fmt.Stringer (String() string method).
	mset := types.NewMethodSet(t)
	for i := 0; i < mset.Len(); i++ {
		m := mset.At(i)
		if m.Obj().Name() != "String" {
			continue
		}
		sig, ok := m.Obj().Type().(*types.Signature)
		if !ok {
			continue
		}
		if sig.Params().Len() == 0 && sig.Results().Len() == 1 {
			if b, ok := sig.Results().At(0).Type().(*types.Basic); ok && b.Kind() == types.String {
				return true
			}
		}
	}
	return false
}

func checkCallArguments(global *Global, fn *types.Signature, args []types.Type) (types.Type, error) {
	if exp, got := fn.Params().Len(), len(args); !fn.Variadic() && exp != got {
		return nil, fmt.Errorf("wrong number of args expected %d but got %d", exp, got)
	}
	expNumFixed := fn.Params().Len()
	isVar := fn.Variadic()
	if isVar {
		expNumFixed--
	}
	got := len(args)
	for i := 0; i < expNumFixed; i++ {
		if i >= len(args) {
			return nil, fmt.Errorf("wrong number of args expected %d but got %d", expNumFixed, got)
		}
		pt := fn.Params().At(i).Type()
		at := args[i]
		assignable := types.AssignableTo(at, pt)
		if !assignable {
			if ptr, ok := at.Underlying().(*types.Pointer); ok {
				if types.AssignableTo(ptr.Elem(), pt) {
					return pt, nil
				}
			}
			if ptr, ok := pt.Underlying().(*types.Pointer); ok {
				if types.AssignableTo(at, ptr.Elem()) {
					return pt, nil
				}
			}
			return nil, fmt.Errorf("argument %d has type %s expected %s", i, global.TypeString(at), global.TypeString(pt))
		}
	}
	if isVar {
		pt := fn.Params().At(fn.Params().Len() - 1).Type().(*types.Slice).Elem()
		for i := expNumFixed; i < len(args); i++ {
			at := args[i]
			assignable := types.AssignableTo(at, pt)
			if !assignable {
				if ptr, ok := at.Underlying().(*types.Pointer); ok {
					if types.AssignableTo(ptr.Elem(), pt) {
						return pt, nil
					}
				}
				if ptr, ok := pt.Underlying().(*types.Pointer); ok {
					if types.AssignableTo(at, ptr.Elem()) {
						return pt, nil
					}
				}
				return nil, fmt.Errorf("argument %d has type %s expected %s", i, global.TypeString(at), global.TypeString(pt))
			}
		}
	}
	return fn.Results().At(0).Type(), nil
}

func findPackage(pkg *types.Package, path string) (*types.Package, bool) {
	if pkg == nil {
		return nil, false
	}
	if pkg.Path() == path {
		return pkg, true
	}
	for _, im := range pkg.Imports() {
		if p, ok := findPackage(im, path); ok {
			return p, true
		}
	}
	return nil, false
}

func builtInCheck(global *Global, funcIdent string, nodes []parse.Node, argTypes []types.Type) (types.Type, error) {
	switch funcIdent {
	case "attrescaper":
		return types.Universe.Lookup("string").Type(), nil
	case "len":
		switch x := argTypes[0].Underlying().(type) {
		default:
			return nil, fmt.Errorf("built-in len expects the first argument to be an array, slice, map, or string got %s", global.TypeString(x))
		case *types.Basic:
			if x.Kind() != types.String {
				return nil, fmt.Errorf("built-in len expects the first argument to be an array, slice, map, or string got %s", global.TypeString(x))
			}
		case *types.Array:
		case *types.Slice:
		case *types.Map:
		}
		return types.Universe.Lookup("int").Type(), nil
	case "slice":
		if l := len(argTypes); l < 1 || l > 4 {
			return nil, fmt.Errorf("built-in slice expects at least 1 and no more than 3 arguments got %d", len(argTypes))
		}
		for i := 1; i < len(nodes); i++ {
			if n, ok := nodes[i].(*parse.NumberNode); ok && n.Int64 < 0 {
				return nil, fmt.Errorf("index %s out of bound", n.Text)
			}
		}
		switch x := argTypes[0].Underlying().(type) {
		default:
			return nil, fmt.Errorf("built-in slice expects the first argument to be an array, slice, or string got %s", global.TypeString(x))
		case *types.Basic:
			if x.Kind() != types.String {
				return nil, fmt.Errorf("built-in slice expects the first argument to be an array, slice, or string got %s", global.TypeString(x))
			}
			if len(nodes) == 4 {
				return nil, fmt.Errorf("can not 3 index slice a string")
			}
			return types.Universe.Lookup("string").Type(), nil
		case *types.Array:
			return x.Elem(), nil
		case *types.Slice:
			return x.Elem(), nil
		}
	case "and", "or":
		if len(argTypes) < 1 {
			return nil, fmt.Errorf("built-in eq expects at least two arguments got %d", len(argTypes))
		}
		first := argTypes[0]
		for _, a := range argTypes[1:] {
			if !types.AssignableTo(a, first) {
				return first, nil
			}
		}
		return first, nil
	case "eq", "ge", "gt", "le", "lt", "ne":
		if len(argTypes) < 2 {
			return nil, fmt.Errorf("built-in eq expects at least two arguments got %d", len(argTypes))
		}
		return types.Universe.Lookup("bool").Type(), nil
	case "call":
		if len(argTypes) < 1 {
			return nil, fmt.Errorf("call expected a function argument")
		}
		sig, ok := argTypes[0].(*types.Signature)
		if !ok {
			return nil, fmt.Errorf("call expected a function signature")
		}
		return checkCallArguments(global, sig, argTypes[1:])
	case "not":
		if len(argTypes) < 1 {
			return nil, fmt.Errorf("built-in not expects at least one argument")
		}
		return types.Universe.Lookup("bool").Type(), nil
	case "index":
		result := argTypes[0]
		for i := 1; i < len(argTypes); i++ {
			at := argTypes[i]
			result = dereference(result)
			switch x := result.(type) {
			case *types.Slice:
				if !types.AssignableTo(at, types.Typ[types.Int]) {
					return nil, fmt.Errorf("slice index expects int got %s", global.TypeString(at))
				}
				result = x.Elem()
			case *types.Array:
				if !types.AssignableTo(at, types.Typ[types.Int]) {
					return nil, fmt.Errorf("slice index expects int got %s", global.TypeString(at))
				}
				result = x.Elem()
			case *types.Map:
				if !types.AssignableTo(at, x.Key()) {
					return nil, fmt.Errorf("slice index expects %s got %s", global.TypeString(x.Key()), global.TypeString(at))
				}
				result = x.Elem()
			default:
				return nil, fmt.Errorf("can not index over %s", global.TypeString(result))
			}
		}
		return result, nil
	default:
		return nil, fmt.Errorf("unknown function: %s", funcIdent)
	}
}
