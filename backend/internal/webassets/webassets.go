// Package webassets carries the built web portal inside the server binary so a
// single executable can serve the whole application with no companion
// directory.
//
// The assets directory is populated by scripts/build.sh immediately before the
// Go build and is not committed. A plain `go build` therefore produces a binary
// with no embedded portal, and the server falls back to locating the assets on
// disk, which keeps the development workflow unchanged.
package webassets

import (
	"embed"
	"io/fs"
)

// The all: prefix keeps files whose names begin with "." or "_", which Vite
// does not currently emit but which a future asset pipeline might.
//
//go:embed all:assets
var embedded embed.FS

// FS returns the embedded web root, or nil when this binary was built without
// the portal. A build without assets still contains the .gitkeep placeholder,
// so presence of index.html is the only reliable signal.
func FS() fs.FS {
	root, err := fs.Sub(embedded, "assets")
	if err != nil {
		return nil
	}
	info, err := fs.Stat(root, "index.html")
	if err != nil || info.IsDir() {
		return nil
	}
	return root
}
