package main

import (
	"net/http"

	"example.com/app/pkg"
)

func main() {
	http.HandleFunc("/", pkg.HandleIndex)
	_ = http.ListenAndServe(":8080", nil)
}
