//go:build frontend

package tracer

import (
	"embed"
	"io/fs"
)

//go:embed frontend/dist
var assets embed.FS

func Frontend() fs.FS {
	frontend, err := fs.Sub(assets, "frontend/dist")
	if err != nil {
		panic(err)
	}
	return frontend
}
