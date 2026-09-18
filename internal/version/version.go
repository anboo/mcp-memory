// Package version carries the build version of the command binaries.
//
// The release workflow injects the value with
//
//	-ldflags "-X github.com/anboo/mcp-memory/internal/version.Version=1.2.3"
//
// so the default here is only used for local, untagged builds.
package version

// Version is the build version. It is "dev" unless set at link time.
var Version = "dev"
