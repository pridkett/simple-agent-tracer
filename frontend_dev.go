//go:build !frontend

package tracer

import (
	"io/fs"
	"os"
)

func Frontend() fs.FS { return os.DirFS("frontend/dist") }
