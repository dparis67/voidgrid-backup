// Package docs holds the documentation pages, embedded into the server binary
// so the UI can show the docs that match the running version.
package docs

import "embed"

//go:embed *.md
var FS embed.FS
