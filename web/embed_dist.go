//go:build embedweb

// Package web holds the built frontend (web/dist) embedded into the server
// binary. Build it first: npm --prefix web run build, then go build with
// -tags embedweb (make build does both).
package web

import (
	"embed"
	"io/fs"
)

//go:embed all:dist
var dist embed.FS

// Dist returns the built frontend and true.
func Dist() (fs.FS, bool) {
	sub, err := fs.Sub(dist, "dist")
	if err != nil {
		panic(err) // the embed pattern above guarantees the directory
	}
	return sub, true
}
