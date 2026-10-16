package httpapi

import (
	"io"
	"io/fs"
	"net/http"
	"strings"
)

// Static serves the built frontend. Files under /assets/ have a content
// hash in their names, so they are cached for good; index.html is
// revalidated on every load, so a new release is picked up at once.
// Without a frontend (fsys == nil) it serves a hint instead.
func Static(fsys fs.FS) http.Handler {
	if fsys == nil {
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, "This server was built without the frontend: use `make build`, or run the Vite dev server (npm --prefix web run dev).\n")
		})
	}
	files := http.FileServerFS(fsys)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/assets/") {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		} else {
			w.Header().Set("Cache-Control", "no-cache")
		}
		files.ServeHTTP(w, r)
	})
}
