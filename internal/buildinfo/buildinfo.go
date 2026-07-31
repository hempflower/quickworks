// Package buildinfo exposes build metadata injected by release builds.
package buildinfo

import "fmt"

var (
	Version   = "dev"
	Commit    = "unknown"
	BuildDate = "unknown"
)

func String() string {
	return fmt.Sprintf("quickworks %s\ncommit: %s\nbuilt: %s", Version, Commit, BuildDate)
}
