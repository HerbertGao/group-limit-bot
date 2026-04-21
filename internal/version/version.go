package version

import (
	"fmt"
	"runtime"
)

// Version / BuildTime / GitCommit are injected at build time via:
//   go build -ldflags "-X github.com/herbertgao/group-limit-bot/internal/version.Version=vX.Y.Z
//                      -X github.com/herbertgao/group-limit-bot/internal/version.BuildTime=<iso8601>
//                      -X github.com/herbertgao/group-limit-bot/internal/version.GitCommit=<sha>"
var (
	Version   = "dev"
	BuildTime = "unknown"
	GitCommit = "unknown"
)

// GetVersion returns the injected version string.
func GetVersion() string { return Version }

// GetFullVersionInfo returns a multi-line summary suitable for `--version` output.
func GetFullVersionInfo() string {
	return fmt.Sprintf(
		"Version:    %s\nBuild Time: %s\nGit Commit: %s\nGo Version: %s\nOS/Arch:    %s/%s",
		Version, BuildTime, GitCommit, runtime.Version(), runtime.GOOS, runtime.GOARCH,
	)
}
