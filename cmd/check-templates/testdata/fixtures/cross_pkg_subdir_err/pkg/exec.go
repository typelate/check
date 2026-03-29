package pkg

import (
	"embed"
	"fmt"
	"html/template"
	"net/http"
)

//go:embed templates/*.gotmpl
var source embed.FS

var templates = template.Must(template.ParseFS(source, "templates/*.gotmpl"))

type Page struct {
	Title string
}

func HandleIndex(w http.ResponseWriter, r *http.Request) {
	_ = templates.ExecuteTemplate(w, "page.gotmpl", Page{Title: "Home"})
}

var _ = fmt.Sprint
