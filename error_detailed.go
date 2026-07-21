package check

import (
	"errors"
	"fmt"
	"go/token"
	"go/types"
	"io"
	"strings"
)

// maxInlineTypeLength bounds how long an inline (unnamed) type may render in
// DetailedError output before it is elided to a summary such as struct{...}.
const maxInlineTypeLength = 64

// DetailedError writes a multi-line rendering of the error tree to sb. Each
// failure block starts with the compact single-line message, followed by
// supporting detail: identifier lookup failures list the receiver type's
// exported fields (with their types) and method signatures one per line;
// call failures show the callee signature and the argument types.
//
// Every line — including the leading location line — prints type names with
// types.WriteType using q, so a caller can qualify packages for the target
// file's import context, for example a language server rendering types with
// the identifiers the importing file declares. A nil q prints full package
// paths, matching Error.
//
// Unlike Error, massive inline composite types are elided: an unnamed
// struct, interface, or signature whose rendering exceeds a small threshold
// prints as struct{...}, interface{...}, or a signature with unnamed
// shortened parameters, with container shape preserved (e.g.
// []struct{...}). The full types remain available on the Error's X field
// and the IdentifierError/CallError causes.
//
// DetailedError returns the first error encountered writing to w, or nil.
func (e *Error) DetailedError(w io.Writer, q types.Qualifier) error {
	sw := &stickyWriter{w: w}
	e.writeDetail(sw, shortTypeFormat(q))
	return sw.err
}

func (e *Error) writeDetail(sw *stickyWriter, tf typeFormatFunc) {
	if len(e.children) > 0 {
		for i, child := range e.children {
			if i > 0 {
				sw.writeString("\n\n")
			}
			child.writeDetail(sw, tf)
		}
		return
	}
	sw.writeString(e.line(tf))
	if identErr, ok := errors.AsType[*IdentifierError](e.err); ok && identErr.Type != nil {
		sw.writeString("\n\n")
		writeTypeMembers(sw, identErr.Type, tf)
		return
	}
	if callErr, ok := errors.AsType[*CallError](e.err); ok && callErr.Signature != nil {
		writeCallDetail(sw, callErr, tf)
	}
}

// stickyWriter forwards writes to w until one fails, then drops the rest and
// keeps the first error.
type stickyWriter struct {
	w   io.Writer
	err error
}

func (sw *stickyWriter) writeString(s string) {
	if sw.err != nil {
		return
	}
	_, sw.err = io.WriteString(sw.w, s)
}

func (sw *stickyWriter) writef(format string, args ...any) {
	if sw.err != nil {
		return
	}
	_, sw.err = fmt.Fprintf(sw.w, format, args...)
}

// shortTypeFormat renders types qualified with q, eliding unnamed composite
// types whose full rendering exceeds maxInlineTypeLength. Container shape
// is preserved so []struct{...} and map[string]struct{...} stay
// recognizable; named types always render in full (their names are short).
func shortTypeFormat(q types.Qualifier) typeFormatFunc {
	var short typeFormatFunc
	short = func(tp types.Type) string {
		full := formatType(tp, q)
		if len(full) <= maxInlineTypeLength {
			return full
		}
		switch t := tp.(type) {
		case *types.Pointer:
			return "*" + short(t.Elem())
		case *types.Slice:
			return "[]" + short(t.Elem())
		case *types.Array:
			return fmt.Sprintf("[%d]%s", t.Len(), short(t.Elem()))
		case *types.Map:
			return "map[" + short(t.Key()) + "]" + short(t.Elem())
		case *types.Chan:
			switch t.Dir() {
			case types.SendOnly:
				return "chan<- " + short(t.Elem())
			case types.RecvOnly:
				return "<-chan " + short(t.Elem())
			default:
				return "chan " + short(t.Elem())
			}
		case *types.Struct:
			return "struct{...}"
		case *types.Interface:
			return "interface{...}"
		case *types.Signature:
			return shortSignature(t, short)
		default:
			return full
		}
	}
	return short
}

// shortSignature renders a signature with shortened, unnamed parameter and
// result types.
func shortSignature(sig *types.Signature, short typeFormatFunc) string {
	var b strings.Builder
	b.WriteString("func(")
	params := sig.Params()
	for i := range params.Len() {
		if i > 0 {
			b.WriteString(", ")
		}
		pt := params.At(i).Type()
		if sig.Variadic() && i == params.Len()-1 {
			if s, ok := pt.(*types.Slice); ok {
				b.WriteString("...")
				b.WriteString(short(s.Elem()))
				continue
			}
		}
		b.WriteString(short(pt))
	}
	b.WriteString(")")
	results := sig.Results()
	switch results.Len() {
	case 0:
	case 1:
		b.WriteString(" ")
		b.WriteString(short(results.At(0).Type()))
	default:
		b.WriteString(" (")
		for i := range results.Len() {
			if i > 0 {
				b.WriteString(", ")
			}
			b.WriteString(short(results.At(i).Type()))
		}
		b.WriteString(")")
	}
	return b.String()
}

// writeTypeMembers writes "T has:" followed by T's exported fields (name and
// type, aligned) in declaration order and then its exported method
// signatures, one per line. A type with no exported members gets
// "T has no exported fields or methods".
func writeTypeMembers(sw *stickyWriter, tp types.Type, tf typeFormatFunc) {
	sw.writeString(tf(tp))
	fields, methods := typeMembers(tp, tf)
	if len(fields) == 0 && len(methods) == 0 {
		sw.writeString(" has no exported fields or methods")
		return
	}
	sw.writeString(" has:")
	width := 0
	for _, f := range fields {
		width = max(width, len(f.name))
	}
	for _, f := range fields {
		sw.writef("\n  %-*s %s", width, f.name, f.detail)
	}
	for _, m := range methods {
		sw.writef("\n  %s%s", m.name, m.detail)
	}
}

func writeCallDetail(sw *stickyWriter, callErr *CallError, tf typeFormatFunc) {
	sw.writeString("\n\n  signature: ")
	if callErr.Name != "" {
		sw.writeString(callErr.Name)
	} else {
		sw.writeString("func")
	}
	sw.writeString(strings.TrimPrefix(tf(callErr.Signature), "func"))
	if len(callErr.ArgTypes) > 0 {
		sw.writeString("\n  arguments:")
		for i, at := range callErr.ArgTypes {
			sw.writef("\n    [%d] %s", i, tf(at))
		}
	}
}

// typeMember pairs a field or method name with its rendered type or
// signature.
type typeMember struct {
	name, detail string
}

// typeMembers collects tp's exported struct fields (including promoted
// fields from embedded structs) and the exported methods in its method set,
// rendered with tf.
func typeMembers(tp types.Type, tf typeFormatFunc) (fields, methods []typeMember) {
	seen := make(map[string]bool)

	// For interfaces, use the type directly (pointer-to-interface has an
	// empty method set). For concrete types, use the pointer to pick up
	// pointer receiver methods.
	var mset *types.MethodSet
	if types.IsInterface(tp) {
		mset = types.NewMethodSet(tp)
	} else {
		mset = types.NewMethodSet(types.NewPointer(tp))
	}
	for i := range mset.Len() {
		obj := mset.At(i).Obj()
		name := obj.Name()
		if !token.IsExported(name) || seen[name] {
			continue
		}
		seen[name] = true
		methods = append(methods, typeMember{
			name:   name,
			detail: strings.TrimPrefix(tf(obj.Type()), "func"),
		})
	}

	if st, ok := tp.Underlying().(*types.Struct); ok {
		collectFieldMembers(st, tf, seen, &fields)
	}
	return fields, methods
}

func collectFieldMembers(st *types.Struct, tf typeFormatFunc, seen map[string]bool, fields *[]typeMember) {
	for i := range st.NumFields() {
		f := st.Field(i)
		if !f.Exported() {
			continue
		}
		if f.Embedded() {
			// Dereference so fields promoted through an embedded *T are
			// listed too.
			if inner, ok := dereference(f.Type()).Underlying().(*types.Struct); ok {
				collectFieldMembers(inner, tf, seen, fields)
			}
			continue
		}
		if seen[f.Name()] {
			continue
		}
		seen[f.Name()] = true
		*fields = append(*fields, typeMember{name: f.Name(), detail: tf(f.Type())})
	}
}
