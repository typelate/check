package main

import (
	"embed"
	"fmt"
	"html/template"
	"net/http"
)

//go:embed *.gotmpl
var source embed.FS

var templates = template.Must(template.ParseFS(source, "*.gotmpl"))

type Page struct {
	Title string
}

func handleIndex(w http.ResponseWriter, r *http.Request) {
	_ = templates.ExecuteTemplate(w, "page.gotmpl", Page{Title: "Home"})
}

var _ = fmt.Sprint
