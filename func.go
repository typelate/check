package check

import (
	"fmt"
	"go/types"
	"maps"
	"text/template/parse"
)

type Functions map[string]*types.Signature

// DefaultFunctions returns the standard functions defined in both html/template and text/template.
func DefaultFunctions(pkg *types.Package) Functions {
	fns := make(map[string]*types.Signature)
	for pn, idents := range map[string]map[string]string{
		"fmt": {
			"Sprint":   "print",
			"Sprintf":  "printf",
			"Sprintln": "println",
		},
		"html/template": {
			"URLQueryEscaper": "urlescaper",
			"HTMLEscaper":     "htmlescaper",
		},
		"text/template": {
			"JSEscaper":       "js",
			"URLQueryEscaper": "urlquery",
			"HTMLEscaper":     "html",
		},
	} {
		if p, ok := findPackage(pkg, pn); ok && p != nil {
			for funcIdent, templateFunc := range idents {
				fns[templateFunc] = p.Scope().Lookup(funcIdent).Type().(*types.Signature)
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

func (functions Functions) CheckCall(funcIdent string, argNodes []parse.Node, argTypes []types.Type) (types.Type, error) {
	fn, ok := functions[funcIdent]
	if !ok {
		return builtInCheck(funcIdent, argNodes, argTypes)
	}
	if resultLen := fn.Results().Len(); resultLen == 0 {
		return nil, fmt.Errorf("function %s has no results", funcIdent)
	} else if resultLen > 2 {
		return nil, fmt.Errorf("function %s has too many results", funcIdent)
	}
	return checkCallArguments(fn, argTypes)
}

func checkCallArguments(fn *types.Signature, args []types.Type) (types.Type, error) {
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
			return nil, fmt.Errorf("argument %d has type %s expected %s", i, at, pt)
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
				return nil, fmt.Errorf("argument %d has type %s expected %s", i, at, pt)
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

func builtInCheck(funcIdent string, nodes []parse.Node, argTypes []types.Type) (types.Type, error) {
	switch funcIdent {
	case "attrescaper":
		return types.Universe.Lookup("string").Type(), nil
	case "len":
		switch x := argTypes[0].Underlying().(type) {
		default:
			return nil, fmt.Errorf("built-in len expects the first argument to be an array, slice, map, or string got %s", x.String())
		case *types.Basic:
			if x.Kind() != types.String {
				return nil, fmt.Errorf("built-in len expects the first argument to be an array, slice, map, or string got %s", x.String())
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
			return nil, fmt.Errorf("built-in slice expects the first argument to be an array, slice, or string got %s", x.String())
		case *types.Basic:
			if x.Kind() != types.String {
				return nil, fmt.Errorf("built-in slice expects the first argument to be an array, slice, or string got %s", x.String())
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
		return checkCallArguments(sig, argTypes[1:])
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
					return nil, fmt.Errorf("slice index expects int got %s", at)
				}
				result = x.Elem()
			case *types.Array:
				if !types.AssignableTo(at, types.Typ[types.Int]) {
					return nil, fmt.Errorf("slice index expects int got %s", at)
				}
				result = x.Elem()
			case *types.Map:
				if !types.AssignableTo(at, x.Key()) {
					return nil, fmt.Errorf("slice index expects %s got %s", x.Key(), at)
				}
				result = x.Elem()
			default:
				return nil, fmt.Errorf("can not index over %s", result)
			}
		}
		return result, nil
	default:
		return nil, fmt.Errorf("unknown function: %s", funcIdent)
	}
}
