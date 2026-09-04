package check

import (
	"go/token"
	"reflect"
	"strings"
	"text/template/parse"
)

// ParseNodePosition reports where n sits in tree following go/token
// semantics: Line and Column are one based and Column counts bytes from
// the start of the line. The filename is the tree's ParseName.
//
// The tree does not export the text it was parsed from, so the position
// is derived from the private text field by reflection.
func ParseNodePosition(tree *parse.Tree, n parse.Node) token.Position {
	pos := int(n.Position())
	fullText := reflect.ValueOf(tree).Elem().FieldByName("text").String()
	text := fullText[:pos]
	lineStart := strings.LastIndexByte(text, '\n') + 1
	return token.Position{
		Filename: tree.ParseName,
		Offset:   pos,
		Line:     1 + strings.Count(text, "\n"),
		Column:   pos - lineStart + 1,
	}
}
