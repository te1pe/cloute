//go:build !embedweb

// Package web holds the built frontend (web/dist) embedded into the server
// binary. Without the embedweb tag nothing is embedded, so that go build and
// go test work before the frontend is built; during development the Vite dev
// server serves it.
package web

import "io/fs"

// Dist returns false: this binary has no frontend.
func Dist() (fs.FS, bool) {
	return nil, false
}
