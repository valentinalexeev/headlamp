package core

import "strings"

// Version is the headlamp release this binary was built from. The image build sets it from the repo's VERSION file
// (-ldflags -X); a local build without it says "dev".
var Version = "dev"

// DisplayVersion is the version as the console shows it: "v0.10.0", or "dev" for a local build.
func DisplayVersion() string {
	v := strings.TrimSpace(Version)
	if v == "" || v == "dev" {
		return "dev"
	}
	return "v" + strings.TrimPrefix(v, "v")
}
