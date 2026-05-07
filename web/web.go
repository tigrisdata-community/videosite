package web

import "embed"

//go:generate go tool templ generate

//go:embed static
var Static embed.FS
