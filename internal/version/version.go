// Package version holds the build version, set at link time with
// -ldflags "-X github.com/wizz666/nestward/internal/version.Version=..."
package version

// Version is the program version. "dev" for local builds.
var Version = "dev"
