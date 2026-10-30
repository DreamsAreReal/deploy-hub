package crawler

import (
	"embed"
	"io/fs"
	"path"
)

const (
	File   = "crawl.mjs"
	suffix = ".mjs"
)

//go:embed *.mjs
var modules embed.FS

type Module struct {
	Name string
	Body []byte
}

func Modules() ([]Module, error) {
	return read(modules)
}

func read(src fs.FS) ([]Module, error) {
	entries, err := fs.ReadDir(src, ".")
	if err != nil {
		return nil, err
	}
	var out []Module
	for _, entry := range entries {
		if path.Ext(entry.Name()) != suffix {
			continue
		}
		body, err := fs.ReadFile(src, entry.Name())
		if err != nil {
			return nil, err
		}
		out = append(out, Module{Name: entry.Name(), Body: body})
	}
	return out, nil
}
