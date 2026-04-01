package graft

import (
	"fmt"
	"runtime/debug"
	"slices"
	"strings"

	graftv1 "github.com/edaniels/graft/gen/proto/graft/v1"
)

// buildVersion is set at build time via ldflags to override the version
// reported by debug.ReadBuildInfo (which only works for `go install pkg@version`).
//
//nolint:gochecknoglobals
var buildVersion string

// ourVersion is a GLOBAL for keeping track of a graft based binary's version info.
var (
	ourBuildInfo   *debug.BuildInfo
	ourBuildInfoOk bool
	ourVersion     *graftv1.VersionInfo
)

func init() {
	ourBuildInfo, ourBuildInfoOk = debug.ReadBuildInfo()
	ourVersion = BuildVersion()
}

// this string shows up in golang debug build output and we match against it.
const goDevelVersionString = "(devel)"

// VersionIsInSourceTree is used to check if this process is running from within the source tree.
// Uses the raw build info rather than ourVersion so tests can override ourVersion without
// affecting source tree detection.
func VersionIsInSourceTree() bool {
	return ourBuildInfoOk && ourBuildInfo.Main.Version == goDevelVersionString
}

// BuildVersion utilizes golang build-time debug info to return a version info object.
//
// TODO(erd): verify we can get this info even if we start stripping out some debug info
// for security reasons.
func BuildVersion() *graftv1.VersionInfo {
	var info graftv1.VersionInfo
	if !ourBuildInfoOk {
		info.Notes = new("unable to determine version info")

		return &info
	}

	if buildVersion != "" {
		info.Version = &buildVersion
	} else if ourBuildInfo.Main.Version != "" {
		info.Version = &ourBuildInfo.Main.Version
	}

	if vIdx := slices.IndexFunc(ourBuildInfo.Settings, func(bs debug.BuildSetting) bool {
		return bs.Key == "vcs.revision"
	}); vIdx != -1 {
		info.VcsRevision = &ourBuildInfo.Settings[vIdx].Value
	}

	if vIdx := slices.IndexFunc(ourBuildInfo.Settings, func(bs debug.BuildSetting) bool {
		return bs.Key == "vcs.time"
	}); vIdx != -1 {
		info.VcsTime = &ourBuildInfo.Settings[vIdx].Value
	}

	if vIdx := slices.IndexFunc(ourBuildInfo.Settings, func(bs debug.BuildSetting) bool {
		return bs.Key == "vcs.modified"
	}); vIdx != -1 {
		modified := ourBuildInfo.Settings[vIdx].Value == "true"
		info.VcsModified = &modified
	}

	return &info
}

// VersionString returns a human-readable version string for CLI output.
func VersionString() string {
	return versionString(BuildVersion())
}

func versionString(info *graftv1.VersionInfo) string {
	// Use tagged version if available and not a pseudo-version
	if info.Version != nil {
		ver := info.GetVersion()
		if ver != "" && ver != goDevelVersionString && !strings.HasPrefix(ver, "v0.0.0-") {
			return ver
		}
	}

	// Fall back to VCS revision for dev builds
	if info.VcsRevision != nil {
		rev := info.GetVcsRevision()
		if len(rev) > 7 {
			rev = rev[:7]
		}

		suffix := ""
		if info.VcsModified != nil && info.GetVcsModified() {
			suffix = "-dirty"
		}

		return fmt.Sprintf("dev-%s%s", rev, suffix)
	}

	if info.Notes != nil {
		return info.GetNotes()
	}

	return "unknown"
}

// BuildVersionsEqual compares two versions and returns an empty string if they are equal and if they are
// not, a non-empty string explaining why they are not equal.
func BuildVersionsEqual(infoA, infoB *graftv1.VersionInfo) string {
	if infoA.Version != nil && infoB.Version != nil {
		if infoA.GetVersion() == infoB.GetVersion() {
			if infoA.GetVersion() == goDevelVersionString {
				return "development build"
			}

			return ""
		}

		return "different versions"
	}

	if (infoA.VcsModified != nil && infoA.GetVcsModified()) || (infoB.VcsModified != nil && infoB.GetVcsModified()) {
		return "dirty vcs build"
	}

	if infoA.VcsRevision != nil && infoB.VcsRevision != nil {
		if infoA.GetVcsRevision() != infoB.GetVcsRevision() {
			return "different vcs revisions"
		}

		return ""
	}

	// One side has version/revision info the other lacks; can't confirm they match.
	if infoA.Version != nil || infoB.Version != nil ||
		infoA.VcsRevision != nil || infoB.VcsRevision != nil {
		return "version info mismatch"
	}

	return ""
}
