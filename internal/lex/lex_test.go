package lex

import (
	"fmt"
	"strings"
	"testing"
)

// formatSpan renders the text a span covers along with its offset, so a
// test table documents both what was matched and where.
func formatSpan(text string, s Span) string {
	if s.Off < 0 {
		return "-"
	}
	return fmt.Sprintf("%q@%d", text[s.Off:s.Off+s.Len], s.Off)
}

func formatDefinition(text string, d Clause) string {
	return fmt.Sprintf("%q define=%s name=%s end=%s",
		d.Name,
		formatSpan(text, d.Define),
		formatSpan(text, d.NameLiteral),
		formatSpan(text, d.End),
	)
}

func TestDefinitions(t *testing.T) {
	for _, tt := range []struct {
		name        string
		text        string
		left, right string
		want        []string
	}{
		{
			name: "a single define",
			text: `{{define "a"}}hi{{end}}`,
			want: []string{`"a" define="{{define \"a\"}}"@0 name="\"a\""@9 end="{{end}}"@16`},
		},
		{
			name: "a block defines a template too",
			text: `{{block "b" .}}hi{{end}}`,
			want: []string{`"b" define="{{block \"b\" .}}"@0 name="\"b\""@8 end="{{end}}"@17`},
		},
		{
			name:  "custom delimiters",
			text:  `[[define "a"]]hi[[end]]`,
			left:  "[[",
			right: "]]",
			want:  []string{`"a" define="[[define \"a\"]]"@0 name="\"a\""@9 end="[[end]]"@16`},
		},
		{
			name: "trim markers belong to the clause they decorate",
			text: "{{define \"a\" -}}\n  hi\n{{- end}}",
			want: []string{`"a" define="{{define \"a\" -}}"@0 name="\"a\""@9 end="{{- end}}"@22`},
		},
		{
			name: "an empty body still has a matching end",
			text: `{{define "a"}}{{end}}`,
			want: []string{`"a" define="{{define \"a\"}}"@0 name="\"a\""@9 end="{{end}}"@14`},
		},
		{
			name: "a nested if does not steal the end",
			text: `{{define "a"}}{{if .X}}y{{end}}{{end}}`,
			want: []string{`"a" define="{{define \"a\"}}"@0 name="\"a\""@9 end="{{end}}"@31`},
		},
		{
			name: "else does not change nesting depth",
			text: `{{define "a"}}{{if .X}}y{{else}}z{{end}}{{end}}`,
			want: []string{`"a" define="{{define \"a\"}}"@0 name="\"a\""@9 end="{{end}}"@40`},
		},
		{
			name: "nested range and with each consume an end",
			text: `{{define "a"}}{{range .X}}{{with .Y}}z{{end}}{{end}}{{end}}`,
			want: []string{`"a" define="{{define \"a\"}}"@0 name="\"a\""@9 end="{{end}}"@52`},
		},
		{
			// The template lexer ends an identifier at the first byte
			// that cannot continue one, so these open a block just as
			// {{if .X}} does and must consume their own end clause.
			name: "a block keyword followed immediately by a field",
			text: `{{define "a"}}{{if.X}}y{{end}}{{end}}`,
			want: []string{`"a" define="{{define \"a\"}}"@0 name="\"a\""@9 end="{{end}}"@30`},
		},
		{
			name: "a range keyword followed immediately by a field",
			text: `{{define "a"}}{{range.X}}y{{end}}{{end}}`,
			want: []string{`"a" define="{{define \"a\"}}"@0 name="\"a\""@9 end="{{end}}"@33`},
		},
		{
			name: "a block keyword followed immediately by a parenthesis",
			text: `{{define "a"}}{{if(eq 1 1)}}y{{end}}{{end}}`,
			want: []string{`"a" define="{{define \"a\"}}"@0 name="\"a\""@9 end="{{end}}"@36`},
		},
		{
			name: "a right delimiter inside a quoted argument is not a delimiter",
			text: `{{define "a"}}{{if eq .X "}}"}}y{{end}}{{end}}`,
			want: []string{`"a" define="{{define \"a\"}}"@0 name="\"a\""@9 end="{{end}}"@39`},
		},
		{
			name: "a left delimiter inside a raw quoted argument is not a delimiter",
			text: "{{define \"a\"}}{{if eq .X `{{`}}y{{end}}{{end}}",
			want: []string{`"a" define="{{define \"a\"}}"@0 name="\"a\""@9 end="{{end}}"@39`},
		},
		{
			name: "a define inside a comment is not a Clause",
			text: `{{/* {{define "fake"}} */}}{{define "real"}}x{{end}}`,
			want: []string{`"real" define="{{define \"real\"}}"@27 name="\"real\""@36 end="{{end}}"@45`},
		},
		{
			// A comment may carry a trim marker. Unless it is recognised
			// as a comment the scan stops at the first right delimiter in
			// the body, and the comment's own end clause is then mistaken
			// for the define's.
			name: "a left trimmed comment hiding a delimiter and an end clause",
			text: `{{define "a"}}{{- /* a}}b {{end}} c */ -}}body{{end}}`,
			want: []string{`"a" define="{{define \"a\"}}"@0 name="\"a\""@9 end="{{end}}"@46`},
		},
		{
			// Same failure, seen from the other side: a define clause
			// inside a trimmed comment must not become a Clause.
			name: "a left trimmed comment hiding a define clause",
			text: `{{define "a"}}{{- /* x}} {{define "fake"}}y{{end}} */ -}}b{{end}}`,
			want: []string{`"a" define="{{define \"a\"}}"@0 name="\"a\""@9 end="{{end}}"@58`},
		},
		{
			name: "a name containing the right delimiter",
			text: `{{define "a}}b"}}x{{end}}`,
			want: []string{`"a}}b" define="{{define \"a}}b\"}}"@0 name="\"a}}b\""@9 end="{{end}}"@18`},
		},
		{
			name: "a name containing an escaped quote",
			text: `{{define "a\"b"}}x{{end}}`,
			want: []string{`"a\"b" define="{{define \"a\\\"b\"}}"@0 name="\"a\\\"b\""@9 end="{{end}}"@18`},
		},
		{
			name: "a raw quoted name",
			text: "{{define `a`}}x{{end}}",
			want: []string{"\"a\" define=\"{{define `a`}}\"@0 name=\"`a`\"@9 end=\"{{end}}\"@15"},
		},
		{
			name: "sibling definitions are returned in source order",
			text: `{{define "a"}}A{{end}}{{define "b"}}B{{end}}`,
			want: []string{
				`"a" define="{{define \"a\"}}"@0 name="\"a\""@9 end="{{end}}"@15`,
				`"b" define="{{define \"b\"}}"@22 name="\"b\""@31 end="{{end}}"@37`,
			},
		},
		{
			name: "a Clause on the third line",
			text: "\n\n{{define `x`}}{{end}}",
			want: []string{"\"x\" define=\"{{define `x`}}\"@2 name=\"`x`\"@11 end=\"{{end}}\"@16"},
		},
		{
			name: "text without definitions",
			text: `hello {{.X}} world`,
			want: nil,
		},
		{
			name: "a define with no matching end has an invalid end span",
			text: `{{define "a"}}x`,
			want: []string{`"a" define="{{define \"a\"}}"@0 name="\"a\""@9 end=-`},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got := Definitions(tt.text, tt.left, tt.right)

			var formatted []string
			for _, d := range got {
				formatted = append(formatted, formatDefinition(tt.text, d))
			}
			if strings.Join(formatted, "\n") != strings.Join(tt.want, "\n") {
				t.Errorf("Definitions(%q, %q, %q) =\n\t%s\nwant\n\t%s",
					tt.text, tt.left, tt.right,
					strings.Join(formatted, "\n\t"), strings.Join(tt.want, "\n\t"))
			}
		})
	}
}
