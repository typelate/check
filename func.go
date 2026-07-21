package check

import (
	"errors"
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
		return nil, errorf(ErrorTypeBadSignature, "function %s has no results", funcIdent).withX(fn)
	} else if resultLen > 2 {
		return nil, errorf(ErrorTypeBadSignature, "function %s has too many results", funcIdent).withX(fn)
	}
	return checkCallArguments(global, funcIdent, fn, argTypes)
}

func checkCallArguments(global *Global, name string, fn *types.Signature, args []types.Type) (types.Type, error) {
	callErr := func(format string, a ...any) *Error {
		render := renderer(format, a...)
		return wrapError(ErrorTypeCallArguments, nil, nil, &CallError{
			Name:      name,
			Signature: fn,
			ArgTypes:  args,
			Cause:     errors.New(render(nil)),
			render:    render,
			qualifier: global.Qualifier,
		}).withX(fn)
	}

	expNum := fn.Params().Len()
	isVar := fn.Variadic()
	expFixed := expNum
	if isVar {
		expFixed--
	}

	switch {
	case !isVar && expNum != len(args):
		return nil, callErr("wrong number of args expected %d but got %d", expNum, len(args))
	case isVar && len(args) < expFixed:
		return nil, callErr("wrong number of args expected at least %d but got %d", expFixed, len(args))
	}

	for i := 0; i < expFixed; i++ {
		if err := checkArgAssignable(global, callErr, i, fn.Params().At(i).Type(), args[i]); err != nil {
			return nil, err
		}
	}
	if isVar {
		elem := fn.Params().At(expNum - 1).Type().(*types.Slice).Elem()
		for i := expFixed; i < len(args); i++ {
			if err := checkArgAssignable(global, callErr, i, elem, args[i]); err != nil {
				return nil, err
			}
		}
	}
	return fn.Results().At(0).Type(), nil
}

// checkArgAssignable returns nil when at is assignable to pt, allowing one
// level of pointer auto-deref or auto-address (matching template runtime
// semantics). Returns an *Error built via callErr on mismatch.
func checkArgAssignable(global *Global, callErr func(format string, a ...any) *Error, i int, pt, at types.Type) error {
	if types.AssignableTo(at, pt) {
		return nil
	}
	if ptr, ok := at.Underlying().(*types.Pointer); ok && types.AssignableTo(ptr.Elem(), pt) {
		return nil
	}
	if ptr, ok := pt.Underlying().(*types.Pointer); ok && types.AssignableTo(at, ptr.Elem()) {
		return nil
	}
	return callErr("argument %d has type %s expected %s", i, at, pt)
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
		if len(argTypes) < 1 {
			return nil, errorf(ErrorTypeCallArguments, "built-in len expects 1 argument got %d", len(argTypes))
		}
		switch x := argTypes[0].Underlying().(type) {
		default:
			return nil, errorf(ErrorTypeCallArguments, "built-in len expects the first argument to be an array, slice, map, or string got %s", x).withX(x)
		case *types.Basic:
			if x.Kind() != types.String {
				return nil, errorf(ErrorTypeCallArguments, "built-in len expects the first argument to be an array, slice, map, or string got %s", x).withX(x)
			}
		case *types.Array:
		case *types.Slice:
		case *types.Map:
		}
		return types.Universe.Lookup("int").Type(), nil
	case "slice":
		if l := len(argTypes); l < 1 || l > 4 {
			return nil, errorf(ErrorTypeCallArguments, "built-in slice expects between 1 and 4 arguments got %d", len(argTypes))
		}
		for i := 1; i < len(nodes); i++ {
			if n, ok := nodes[i].(*parse.NumberNode); ok && n.Int64 < 0 {
				return nil, errorf(ErrorTypeCallArguments, "index %s out of bound", n.Text)
			}
		}
		switch x := argTypes[0].Underlying().(type) {
		default:
			return nil, errorf(ErrorTypeCallArguments, "built-in slice expects the first argument to be an array, slice, or string got %s", x).withX(x)
		case *types.Basic:
			if x.Kind() != types.String {
				return nil, errorf(ErrorTypeCallArguments, "built-in slice expects the first argument to be an array, slice, or string got %s", x).withX(x)
			}
			if len(nodes) == 4 {
				return nil, errorf(ErrorTypeCallArguments, "can not 3 index slice a string")
			}
			return types.Universe.Lookup("string").Type(), nil
		case *types.Array:
			return x.Elem(), nil
		case *types.Slice:
			return x.Elem(), nil
		}
	case "and", "or":
		if len(argTypes) < 1 {
			return nil, errorf(ErrorTypeCallArguments, "built-in %s expects at least one argument got %d", funcIdent, len(argTypes))
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
			return nil, errorf(ErrorTypeCallArguments, "built-in %s expects at least two arguments got %d", funcIdent, len(argTypes))
		}
		return types.Universe.Lookup("bool").Type(), nil
	case "call":
		if len(argTypes) < 1 {
			return nil, errorf(ErrorTypeCallArguments, "call expected a function argument")
		}
		sig, ok := argTypes[0].(*types.Signature)
		if !ok {
			return nil, errorf(ErrorTypeCallArguments, "call expected a function signature").withX(argTypes[0])
		}
		return checkCallArguments(global, "", sig, argTypes[1:])
	case "not":
		if len(argTypes) < 1 {
			return nil, errorf(ErrorTypeCallArguments, "built-in not expects at least one argument")
		}
		return types.Universe.Lookup("bool").Type(), nil
	case "index":
		if len(argTypes) < 1 {
			return nil, errorf(ErrorTypeCallArguments, "built-in index expects at least 1 argument got %d", len(argTypes))
		}
		result := argTypes[0]
		for i := 1; i < len(argTypes); i++ {
			at := argTypes[i]
			result = dereference(result)
			switch x := result.(type) {
			case *types.Slice:
				if !types.AssignableTo(at, types.Typ[types.Int]) {
					return nil, errorf(ErrorTypeCallArguments, "slice index expects int got %s", at).withX(at)
				}
				result = x.Elem()
			case *types.Array:
				if !types.AssignableTo(at, types.Typ[types.Int]) {
					return nil, errorf(ErrorTypeCallArguments, "slice index expects int got %s", at).withX(at)
				}
				result = x.Elem()
			case *types.Map:
				if !types.AssignableTo(at, x.Key()) {
					return nil, errorf(ErrorTypeCallArguments, "slice index expects %s got %s", x.Key(), at).withX(at)
				}
				result = x.Elem()
			default:
				return nil, errorf(ErrorTypeCallArguments, "can not index over %s", result).withX(result)
			}
		}
		return result, nil
	default:
		return nil, errorf(ErrorTypeUnknownFunction, "unknown function: %s", funcIdent)
	}
}
