package node

import (
	"embed"
	"io/fs"
	"net/http"
)

//go:embed web
var webFiles embed.FS

func registerWeb(mux *http.ServeMux) {
	assets, err := fs.Sub(webFiles, "web")
	if err != nil {
		panic(err)
	}
	files := http.FileServer(http.FS(assets))
	handler := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		// Radix primitives and the auto-growing composer use inline style
		// attributes; scripts remain restricted to same-origin bundles.
		w.Header().Set("Content-Security-Policy", "default-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; object-src 'none'; base-uri 'none'; frame-ancestors 'none'; form-action 'self'")
		w.Header().Set("Cache-Control", "no-store")
		files.ServeHTTP(w, r)
	}
	mux.HandleFunc("GET /assets/", handler)
	mux.HandleFunc("GET /favicon.svg", handler)
	mux.HandleFunc("GET /{$}", handler)
	mux.HandleFunc("GET /connect", func(w http.ResponseWriter, r *http.Request) {
		r.URL.Path = "/"
		handler(w, r)
	})
}
