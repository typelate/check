package check

import (
	"fmt"
	"strings"
	"testing"
)

// formatSpan renders the text a span covers along with its offset, so a
// test table documents both what was matched and where.
func formatSpan(text string, s textSpan) string {
	if s.Off < 0 {
		return "-"
	}
	return fmt.Sprintf("%q@%d", text[s.Off:s.Off+s.Len], s.Off)
}

func formatDefinition(text string, d definition) string {
	return fmt.Sprintf("%q define=%s name=%s end=%s",
		d.Name,
		formatSpan(text, d.Define),
		formatSpan(text, d.NameLit),
		formatSpan(text, d.End),
	)
}

func TestScanDefinitions(t *testing.T) {
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
			name: "a define inside a comment is not a definition",
			text: `{{/* {{define "fake"}} */}}{{define "real"}}x{{end}}`,
			want: []string{`"real" define="{{define \"real\"}}"@27 name="\"real\""@36 end="{{end}}"@45`},
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
			name: "a definition on the third line",
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
			got := scanDefinitions(tt.text, tt.left, tt.right)

			var formatted []string
			for _, d := range got {
				formatted = append(formatted, formatDefinition(tt.text, d))
			}
			if strings.Join(formatted, "\n") != strings.Join(tt.want, "\n") {
				t.Errorf("scanDefinitions(%q, %q, %q) =\n\t%s\nwant\n\t%s",
					tt.text, tt.left, tt.right,
					strings.Join(formatted, "\n\t"), strings.Join(tt.want, "\n\t"))
			}
		})
	}
}
