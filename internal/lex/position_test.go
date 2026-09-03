package lex

import (
	"go/token"
	"testing"
	"text/template/parse"
)

func TestPosition(t *testing.T) {
	for _, tt := range []struct {
		name     string
		location string
		want     token.Position
	}{
		{
			name:     "a column counted from zero becomes one counted from one",
			location: "index.gohtml:1:11",
			want:     token.Position{Filename: "index.gohtml", Line: 1, Column: 12},
		},
		{
			name:     "the first column of a line",
			location: "index.gohtml:3:0",
			want:     token.Position{Filename: "index.gohtml", Line: 3, Column: 1},
		},
		{
			// The name is whatever precedes the line and column, so a
			// drive letter or any other colon in the path survives.
			name:     "a name containing colons",
			location: `C:\tmp\index.gohtml:2:5`,
			want:     token.Position{Filename: `C:\tmp\index.gohtml`, Line: 2, Column: 6},
		},
		{
			name:     "a template named rather than a file",
			location: "app:1:0",
			want:     token.Position{Filename: "app", Line: 1, Column: 1},
		},
		{
			name:     "a location with no line or column at all",
			location: "index.gohtml",
			want:     token.Position{Filename: "index.gohtml"},
		},
		{
			name:     "a location whose trailing fields are not numbers",
			location: "a:b:c",
			want:     token.Position{Filename: "a:b:c"},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := Position(tt.location); got != tt.want {
				t.Errorf("Position(%q) = %+v, want %+v", tt.location, got, tt.want)
			}
		})
	}

	t.Run("a location ErrorContext really produced", func(t *testing.T) {
		// Rather than trust the format, take one from the source of it:
		// {{.Missing}} puts the field at offset 2, which ErrorContext
		// calls column 2 and go/token calls column 3.
		trees, err := parse.Parse("index.gohtml", "{{.Missing}}", "", "", map[string]any{})
		if err != nil {
			t.Fatalf("parsing: %v", err)
		}
		tree := trees["index.gohtml"]
		location, _ := tree.ErrorContext(tree.Root.Nodes[0])

		got := Position(location)
		want := token.Position{Filename: "index.gohtml", Line: 1, Column: 3}
		if got != want {
			t.Errorf("Position(%q) = %+v, want %+v", location, got, want)
		}
	})
}
