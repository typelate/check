// Package lex reads the parts of a template that text/template/parse
// keeps to itself.
//
// A parse.Tree records where a template's body begins but not where the
// clause that opened it was written, and drops the matching {{end}}
// altogether, so a tool that wants to point at a definition has to find
// the clauses itself. Definitions does that by walking template actions.
// Position reads back the location strings parse.Tree.ErrorContext hands
// out.
package lex

import (
	"strconv"
	"strings"
)

const (
	defaultLeftDelim  = "{{"
	defaultRightDelim = "}}"
	leftComment       = "/*"
	rightComment      = "*/"
	trimMarker        = '-'
)

// Span is a byte range within template text, relative to the start of
// the text it was scanned from. An Off below zero marks an absent range.
type Span struct {
	Off int
	Len int
}

// IsValid reports whether the span locates anything.
func (s Span) IsValid() bool { return s.Off >= 0 }

// invalidSpan marks a range that could not be located.
var invalidSpan = Span{Off: -1}

// Clause locates one template definition: the clause that opens it, the
// quoted name inside that clause, and the clause that closes it.
type Clause struct {
	// Name is the defined template's name.
	Name string
	// Define spans the {{define ...}} or {{block ...}} clause, from the
	// left delimiter through the right delimiter, trim markers included.
	Define Span
	// NameLiteral spans the quoted name literal inside Define, quotes
	// included.
	NameLiteral Span
	// End spans the matching {{end}} clause.
	End Span
}

// action is one delimited clause located in template text.
type action struct {
	span Span
	// keyword is the clause's leading word, empty for a comment.
	keyword string
	nameLit Span
	name    string
}

// Definitions locates every define and block clause in text along with
// the {{end}} clause that closes it, in source order.
//
// Scanning is delimiter-aware rather than a plain search: quoted
// arguments may contain delimiters ({{if eq .X "}}"}}) and comments may
// contain whole decoy clauses ({{/* {{define "fake"}} */}}), so clauses
// are located by walking actions rather than by matching text.
//
// Empty delimiters select the template defaults. A definition left open
// by malformed input keeps an invalid End.
func Definitions(text, leftDelim, rightDelim string) []Clause {
	if leftDelim == "" {
		leftDelim = defaultLeftDelim
	}
	if rightDelim == "" {
		rightDelim = defaultRightDelim
	}

	var (
		defs []Clause
		// open holds an index into defs for each enclosing define or
		// block clause, and -1 for any other clause that {{end}} closes.
		open []int
	)
	for pos := 0; pos < len(text); {
		i := strings.Index(text[pos:], leftDelim)
		if i < 0 {
			break
		}
		a, ok := scanAction(text, pos+i, leftDelim, rightDelim)
		if !ok {
			break
		}
		pos = a.span.Off + a.span.Len

		switch a.keyword {
		case "define", "block":
			defs = append(defs, Clause{
				Name:        a.name,
				Define:      a.span,
				NameLiteral: a.nameLit,
				End:         invalidSpan,
			})
			open = append(open, len(defs)-1)
		case "if", "range", "with":
			open = append(open, -1)
		case "end":
			if len(open) == 0 {
				continue
			}
			if i := open[len(open)-1]; i >= 0 {
				defs[i].End = a.span
			}
			open = open[:len(open)-1]
		}
	}
	return defs
}

// scanAction measures the clause whose left delimiter begins at start and
// reports its leading keyword. For a define or block clause it also
// locates the quoted name literal.
func scanAction(text string, start int, left, right string) (action, bool) {
	p := start + len(left)
	if hasLeftTrimMarker(text[p:]) {
		p++
	}
	// The trim marker is a minus followed by white space, so the comment
	// opener can only be found after that space is stepped over.
	p = skipSpace(text, p)
	if strings.HasPrefix(text[p:], leftComment) {
		return scanComment(text, start, p, right)
	}

	keyword := p
	for p < len(text) && continuesIdentifier(text[p]) {
		p++
	}

	a := action{keyword: text[keyword:p], nameLit: invalidSpan}
	if a.keyword == "define" || a.keyword == "block" {
		a.name, a.nameLit, p = scanName(text, skipSpace(text, p))
	}

	for p < len(text) {
		if strings.HasPrefix(text[p:], right) {
			a.span = Span{Off: start, Len: p + len(right) - start}
			return a, true
		}
		// A quoted argument may contain either delimiter, so step over
		// it whole rather than scanning through it.
		if c := text[p]; c == '"' || c == '`' || c == '\'' {
			if lit, err := strconv.QuotedPrefix(text[p:]); err == nil {
				p += len(lit)
				continue
			}
		}
		p++
	}
	return action{}, false
}

// scanComment measures a comment clause, whose body ends at the first
// "*/" rather than at a delimiter.
func scanComment(text string, start, p int, right string) (action, bool) {
	i := strings.Index(text[p+len(leftComment):], rightComment)
	if i < 0 {
		return action{}, false
	}
	p = skipRightTrimMarker(text, p+len(leftComment)+i+len(rightComment))
	if !strings.HasPrefix(text[p:], right) {
		return action{}, false
	}
	return action{
		span:    Span{Off: start, Len: p + len(right) - start},
		nameLit: invalidSpan,
	}, true
}

// scanName reads the quoted template name beginning at p, returning the
// name, the span of its literal, and the offset just past it.
func scanName(text string, p int) (name string, lit Span, next int) {
	quoted, err := strconv.QuotedPrefix(text[p:])
	if err != nil {
		return "", invalidSpan, p
	}
	name, err = strconv.Unquote(quoted)
	if err != nil {
		return "", invalidSpan, p
	}
	return name, Span{Off: p, Len: len(quoted)}, p + len(quoted)
}

// continuesIdentifier reports whether c can continue an identifier.
//
// text/template/parse ends an identifier at the first byte that cannot
// continue one, so a clause keyword has to end there too: {{if.X}} opens
// a block exactly as {{if .X}} does, and reading the keyword as "if.X"
// would leave its {{end}} to close an enclosing clause instead.
//
// parse admits any Unicode letter or digit. Only ASCII is accepted here,
// which ends a keyword early on a multi byte rune — harmless, because
// every keyword that opens or closes a clause is ASCII, so an early end
// can only produce a word that is not one of them.
func continuesIdentifier(c byte) bool {
	return c == '_' ||
		'a' <= c && c <= 'z' ||
		'A' <= c && c <= 'Z' ||
		'0' <= c && c <= '9'
}

// isSpace reports whether c is template white space, matching the
// definition text/template/parse uses when trimming.
func isSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\r' || c == '\n'
}

func skipSpace(text string, p int) int {
	for p < len(text) && isSpace(text[p]) {
		p++
	}
	return p
}

// hasLeftTrimMarker reports whether s opens with the "- " that trims
// white space preceding a clause.
func hasLeftTrimMarker(s string) bool {
	return len(s) >= 2 && s[0] == trimMarker && isSpace(s[1])
}

// skipRightTrimMarker steps over the " -" that trims white space
// following a clause, returning p unchanged when it is absent.
func skipRightTrimMarker(text string, p int) int {
	q := skipSpace(text, p)
	if q > p && q < len(text) && text[q] == trimMarker {
		return q + 1
	}
	return p
}
