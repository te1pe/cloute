//go:build !embedweb

package web

import "io/fs"

func Dist() (fs.FS, bool) {
	return nil, false
}
