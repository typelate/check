package asteval

import (
	html "html/template"
	text "text/template"
)

// NewTemplate creates a Template backed by the appropriate template
// package either: "text/template" or "html/template".
func NewTemplate(pkgPath, name string) Template {
	switch pkgPath {
	case "text/template":
		return &textTemplate{t: text.New(name)}
	case "html/template":
		return &htmlTemplate{t: html.New(name)}
	default:
		return nil
	}
}
