package version

// Version is injected at build time via:
//   go build -ldflags "-X github.com/herbertgao/group-limit-bot/internal/version.Version=vX.Y.Z"
var Version = "dev"
