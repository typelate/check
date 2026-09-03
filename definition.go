package check

import (
	"go/ast"
	"go/token"
	"os"
	"strconv"
	"text/template/parse"
	"unicode/utf8"
)

// Span is a byte range in a source file.
//
// The embedded Position follows go/token semantics: Line and Column are
// both one based and Column counts bytes from the start of the line.
// Note that parse.Tree.ErrorContext reports a zero based column instead,
// so the two are off by one.
type Span struct {
	token.Position

	// Length is the range's size in bytes, measured in the file named by
	// Position.
	Length int
}

// String renders the span as file:line:column+length, or "-" when the
// span locates nothing.
func (s Span) String() string {
	if !s.IsValid() {
		return "-"
	}
	return s.Position.String() + "+" + strconv.Itoa(s.Length)
}

// Definition locates where a template was defined.
//
// A zero Definition reports an unknown location: its spans are invalid,
// which is what an inspector receives for a template that was never
// defined.
type Definition struct {
	// Name is the defined template's name.
	Name string

	// Define spans the {{define "x"}} or {{block "x" .}} clause, from the
	// left delimiter through the right delimiter, trim markers included.
	Define Span

	// End spans the matching {{end}} clause.
	End Span

	// TemplateName spans the quoted name literal inside Define, quotes
	// included. It is invalid for a template that has no define clause.
	TemplateName Span

	// Tree is the defined template's parse tree, saving a caller a
	// second lookup. It is nil in a zero Definition.
	Tree *parse.Tree
}

// definitionSet resolves a template name to the definition that survived
// being defined more than once.
type definitionSet map[string]Definition

// newDefinitionSet collapses definitions, in the order they were parsed,
// to the one that each name resolves to at run time.
func newDefinitionSet(ordered []Definition) definitionSet {
	set := make(definitionSet, len(ordered))
	for _, def := range ordered {
		if prev, ok := set[def.Name]; ok && !displaces(prev, def) {
			continue
		}
		set[def.Name] = def
	}
	return set
}

func (s definitionSet) FindDefinition(name string) (Definition, bool) {
	def, ok := s[name]
	return def, ok
}

// displaces reports whether next replaces prev, following the rule
// text/template applies when it associates a parse tree: a definition
// whose body is empty leaves an existing body in place.
func displaces(prev, next Definition) bool {
	if prev.Tree == nil || next.Tree == nil {
		return true
	}
	return !parse.IsEmptyTree(next.Tree.Root)
}

// definitionsIn returns a Definition for every template that a template
// file defines, with positions resolved against filename.
func definitionsIn(rootName, filename, text, leftDelim, rightDelim string) []Definition {
	return definitions(source{file: textFile(filename, text)}, rootName, text, leftDelim, rightDelim)
}

// definitionsInLiteral returns a Definition for every template that a Go
// string literal defines, with positions resolved to the Go file holding
// the literal so that they address real bytes in that file.
func definitionsInLiteral(fset *token.FileSet, lit *ast.BasicLit, rootName, leftDelim, rightDelim string) []Definition {
	text, err := strconv.Unquote(lit.Value)
	if err != nil {
		return nil
	}
	file := fset.File(lit.Pos())
	if file == nil {
		return nil
	}
	literal, ok := literalSource(file, lit)
	if !ok {
		return nil
	}
	offsets := literalOffsets(literal)
	if offsets == nil {
		return nil
	}
	src := source{file: file, base: file.Offset(lit.Pos()), offsets: offsets}
	return definitions(src, rootName, text, leftDelim, rightDelim)
}

// literalSource returns the literal as it is written in the file.
//
// go/scanner drops the carriage returns from a raw string literal, so its
// value is shorter than the bytes it was read from, while positions keep
// addressing the file. Mapping a value offset back onto a real byte
// therefore has to walk the file's own bytes. Only a literal whose two
// lengths disagree needs them, so the ordinary file is never re-read.
func literalSource(file *token.File, lit *ast.BasicLit) (string, bool) {
	start, end := file.Offset(lit.Pos()), file.Offset(lit.End())
	if end-start == len(lit.Value) {
		return lit.Value, true
	}
	b, err := os.ReadFile(file.Name())
	if err != nil || start < 0 || end > len(b) {
		return "", false
	}
	literal := string(b[start:end])
	// The file may have changed since it was parsed, in which case these
	// offsets address something else entirely.
	if len(literal) == 0 || len(lit.Value) == 0 || literal[0] != lit.Value[0] {
		return "", false
	}
	return literal, true
}

// definitions locates every template text defines.
//
// The first result is always rootName, the template that owns text
// itself. It has no define clause, so it spans the whole of text: Define
// is empty at the start and End is empty at the end. Definitions written
// with a define or block clause follow in source order.
func definitions(src source, rootName, text, leftDelim, rightDelim string) []Definition {
	trees := parseTrees(rootName, text, leftDelim, rightDelim)
	defs := []Definition{{
		Name:   rootName,
		Define: src.span(textSpan{Off: 0}),
		End:    src.span(textSpan{Off: len(text)}),
		Tree:   trees[rootName],
	}}
	for _, d := range scanDefinitions(text, leftDelim, rightDelim) {
		defs = append(defs, Definition{
			Name:         d.Name,
			Define:       src.span(d.Define),
			End:          src.span(d.End),
			TemplateName: src.span(d.NameLit),
			Tree:         trees[d.Name],
		})
	}
	return defs
}

// parseTrees reparses text to recover each defined template's own parse
// tree, which is the tree that definition would contribute if it won.
// Taking the trees from the finished template instead would hand every
// definition of a name the one that survived, leaving no way to tell
// which of them that was.
//
// Function names are not checked: the caller already parsed this text
// with the real function map, and that map is not available here.
func parseTrees(rootName, text, leftDelim, rightDelim string) map[string]*parse.Tree {
	root := parse.New(rootName)
	root.Mode = parse.SkipFuncCheck
	trees := make(map[string]*parse.Tree)
	if _, err := root.Parse(text, leftDelim, rightDelim, trees); err != nil {
		return nil
	}
	return trees
}

// source resolves byte offsets in template text to positions in the file
// the text was read from.
//
// Template text read from a template file corresponds to that file byte
// for byte. Template text written as a Go string literal does not: it
// begins partway into the file and its escape sequences are wider in the
// source than in the value, so offsets there are translated.
type source struct {
	file *token.File

	// base is the offset of the template text within file.
	base int

	// offsets maps each byte offset in the template text to its offset
	// within the file, relative to base. It is nil when the text and the
	// file correspond byte for byte.
	offsets []int
}

// span resolves a range of template text. A range that cannot be located
// resolves to the zero Span, which reports itself invalid.
func (s source) span(t textSpan) Span {
	start, ok := s.offset(t.Off)
	if !ok {
		return Span{}
	}
	end, ok := s.offset(t.Off + t.Len)
	if !ok {
		return Span{}
	}
	return Span{Position: s.file.Position(s.file.Pos(start)), Length: end - start}
}

// offset translates a byte offset in the template text to one in the
// file. The offset one past the end of the text is valid, so that a
// range can address the end of a template.
func (s source) offset(text int) (int, bool) {
	switch {
	case text < 0:
		return 0, false
	case s.offsets == nil:
		if text > s.file.Size() {
			return 0, false
		}
		return s.base + text, true
	case text >= len(s.offsets):
		return 0, false
	default:
		return s.base + s.offsets[text], true
	}
}

// textFile builds a token.File over template text so that byte offsets
// convert to positions using go/token's own line and column rules.
func textFile(filename, text string) *token.File {
	file := token.NewFileSet().AddFile(filename, -1, len(text))
	// SetLinesForContent records no lines at all for empty content,
	// which would leave every position in an empty template invalid.
	// AddFile's own first line already covers that case.
	if len(text) > 0 {
		file.SetLinesForContent([]byte(text))
	}
	return file
}

// literalOffsets maps every byte offset in a Go string literal's decoded
// value to its byte offset within lit, the literal's source text, and
// ends with the offset just past the value. It reports nil for a literal
// it cannot decode.
//
// The two differ because an escape sequence is wider in the source than
// in the value: every byte a multi byte rune decodes to shares the
// offset of the escape that produced it.
func literalOffsets(lit string) []int {
	if len(lit) < 2 {
		return nil
	}
	quote, body := lit[0], lit[1:len(lit)-1]
	offsets := make([]int, 0, len(body)+1)

	if quote == '`' {
		// Carriage returns are dropped from raw string literals.
		for i := range len(body) {
			if body[i] != '\r' {
				offsets = append(offsets, 1+i)
			}
		}
		return append(offsets, 1+len(body))
	}
	if quote != '"' {
		return nil
	}
	for rest := body; len(rest) > 0; {
		at := 1 + len(body) - len(rest)
		r, multibyte, tail, err := strconv.UnquoteChar(rest, quote)
		if err != nil {
			return nil
		}
		width := 1
		if multibyte {
			if width = utf8.RuneLen(r); width < 0 {
				return nil
			}
		}
		for range width {
			offsets = append(offsets, at)
		}
		rest = tail
	}
	return append(offsets, 1+len(body))
}
