// Package buildinfo reports which build is running (F6 / SA-3). The git commit is injected at link time:
//
//	go build -ldflags "-X github.com/Diansalas/igaming-platform/internal/buildinfo.Commit=<40-hex sha>" ./cmd/platform-api
//
// (deploy/docker/platform-api.Dockerfile does it from the GIT_COMMIT build argument). With no injection the
// toolchain's own VCS stamp (runtime/debug build settings, present when built from a git checkout) is used, and
// otherwise the value is "unknown". Whatever is injected is validated: only 7 to 64 lower-case hexadecimal
// characters are ever reported, so a malformed or hostile build argument can never reach an API response.
package buildinfo

import (
	"regexp"
	"runtime/debug"
)

// Unknown is reported when no valid commit is available.
const Unknown = "unknown"

// Commit is set by -ldflags -X. It is a package variable only because the linker can set nothing else.
var Commit = Unknown

var commitPattern = regexp.MustCompile(`^[0-9a-f]{7,64}$`)

// CommitID returns the validated commit of the running binary, or "unknown".
func CommitID() string {
	return resolveCommit(Commit, vcsRevision())
}

func resolveCommit(injected, vcs string) string {
	if commitPattern.MatchString(injected) {
		return injected
	}
	if commitPattern.MatchString(vcs) {
		return vcs
	}
	return Unknown
}

func vcsRevision() string {
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return ""
	}
	for _, s := range bi.Settings {
		if s.Key == "vcs.revision" {
			return s.Value
		}
	}
	return ""
}
