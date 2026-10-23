package cirunner

import (
	"embed"
	"io/fs"
)

//go:embed ci-* rootlesskit.apparmor var-lib-ci.mount
var files embed.FS

func Files() fs.FS {
	return files
}
