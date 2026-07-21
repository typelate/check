package check

import (
	"bytes"
	"errors"
	"fmt"
	"go/token"
	"go/types"
	"strings"
)

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
// DetailedError always returns nil; the error result exists so callers can
// treat it like an io-style write.
func (e *Error) DetailedError(sb *strings.Builder, q types.Qualifier) error {
	if len(e.children) > 0 {
		for i, child := range e.children {
			if i > 0 {
				sb.WriteString("\n\n")
			}
			if err := child.DetailedError(sb, q); err != nil {
				return err
			}
		}
		return nil
	}
	sb.WriteString(e.line(q))
	var identErr *IdentifierError
	if errors.As(e.err, &identErr) && identErr.Type != nil {
		sb.WriteString("\n\n")
		writeTypeMembers(sb, identErr.Type, q)
		return nil
	}
	var callErr *CallError
	if errors.As(e.err, &callErr) && callErr.Signature != nil {
		writeCallDetail(sb, callErr, q)
	}
	return nil
}

// writeTypeMembers writes "T has:" followed by T's exported fields (name and
// type, aligned) in declaration order and then its exported method
// signatures, one per line. A type with no exported members gets
// "T has no exported fields or methods".
func writeTypeMembers(sb *strings.Builder, tp types.Type, q types.Qualifier) {
	sb.WriteString(formatType(tp, q))
	fields, methods := typeMembers(tp, q)
	if len(fields) == 0 && len(methods) == 0 {
		sb.WriteString(" has no exported fields or methods")
		return
	}
	sb.WriteString(" has:")
	width := 0
	for _, f := range fields {
		width = max(width, len(f.name))
	}
	for _, f := range fields {
		_, _ = fmt.Fprintf(sb, "\n  %-*s %s", width, f.name, f.detail)
	}
	for _, m := range methods {
		_, _ = fmt.Fprintf(sb, "\n  %s%s", m.name, m.detail)
	}
}

func writeCallDetail(sb *strings.Builder, callErr *CallError, q types.Qualifier) {
	sb.WriteString("\n\n  signature: ")
	if callErr.Name != "" {
		sb.WriteString(callErr.Name)
	} else {
		sb.WriteString("func")
	}
	var buf bytes.Buffer
	types.WriteSignature(&buf, callErr.Signature, q)
	sb.Write(buf.Bytes())
	if len(callErr.ArgTypes) > 0 {
		sb.WriteString("\n  arguments:")
		for i, at := range callErr.ArgTypes {
			_, _ = fmt.Fprintf(sb, "\n    [%d] %s", i, formatType(at, q))
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
// rendered with q.
func typeMembers(tp types.Type, q types.Qualifier) (fields, methods []typeMember) {
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
			detail: strings.TrimPrefix(formatType(obj.Type(), q), "func"),
		})
	}

	if st, ok := tp.Underlying().(*types.Struct); ok {
		collectFieldMembers(st, q, seen, &fields)
	}
	return fields, methods
}

func collectFieldMembers(st *types.Struct, q types.Qualifier, seen map[string]bool, fields *[]typeMember) {
	for i := range st.NumFields() {
		f := st.Field(i)
		if !f.Exported() {
			continue
		}
		if f.Embedded() {
			if inner, ok := f.Type().Underlying().(*types.Struct); ok {
				collectFieldMembers(inner, q, seen, fields)
			}
			continue
		}
		if seen[f.Name()] {
			continue
		}
		seen[f.Name()] = true
		*fields = append(*fields, typeMember{name: f.Name(), detail: formatType(f.Type(), q)})
	}
}
