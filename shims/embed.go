// Package shims embeds the small worker scripts that put a Jev-compatible API
// in front of models that don't have one.
package shims

import "embed"

//go:embed */server.py
var FS embed.FS

func Source(name string) ([]byte, error) { return FS.ReadFile(name + "/server.py") }
