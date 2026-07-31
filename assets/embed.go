// Package assets exposes administrator-reviewed workspace bootstrap resources.
package assets

import "embed"

// Files contains the scripts that are downloaded by a newly provisioned VM.
//
//go:embed workspace-bootstrap.sh workspace-startup.sh.tmpl
var Files embed.FS
