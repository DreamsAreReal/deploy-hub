package crawler

import _ "embed"

const File = "crawl.mjs"

//go:embed crawl.mjs
var Script []byte
