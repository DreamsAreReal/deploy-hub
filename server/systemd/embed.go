package systemd

import (
	"embed"
	"io/fs"
)

//go:embed *.service *.timer
var files embed.FS

func Files() fs.FS {
	return files
}
