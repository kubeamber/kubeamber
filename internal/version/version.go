// Package version reports the build version, set at link time with
// -ldflags "-X github.com/kubeamber/kubeamber/internal/version.Version=v0.2.0".
// A binary built by `go install ...@v0.2.0` reports its module version instead.
package version

import "runtime/debug"

var Version = "dev"

func init() {
	if Version != "dev" {
		return
	}
	if bi, ok := debug.ReadBuildInfo(); ok && bi.Main.Version != "" && bi.Main.Version != "(devel)" {
		Version = bi.Main.Version
	}
}
