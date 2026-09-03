package lex

import (
	"go/token"
	"strconv"
	"strings"
)

// Position reads a location as parse.Tree.ErrorContext writes one,
// "name:line:column", and reports it as a token.Position.
//
// ErrorContext counts columns from zero, so a position taken from a
// template reads one column to the left of the same position taken from
// a Go file unless it is adjusted. Position counts from one, so that
// both read alike and an editor sent to either lands on the same byte.
//
// The name may itself contain colons, a Windows path for instance, so
// the line and column are taken from the right. A string that does not
// end in a line and column becomes a filename on its own, which reports
// itself invalid.
func Position(location string) token.Position {
	rest, column, ok := cutTrailingInt(location)
	if !ok {
		return token.Position{Filename: location}
	}
	name, line, ok := cutTrailingInt(rest)
	if !ok {
		return token.Position{Filename: location}
	}
	return token.Position{Filename: name, Line: line, Column: column + 1}
}

// cutTrailingInt splits a trailing ":<number>" off s, reporting whether
// there was one.
func cutTrailingInt(s string) (rest string, n int, ok bool) {
	i := strings.LastIndex(s, ":")
	if i < 0 {
		return s, 0, false
	}
	n, err := strconv.Atoi(s[i+1:])
	if err != nil {
		return s, 0, false
	}
	return s[:i], n, true
}
