package graft

import (
	"os/exec"
	"strings"
	"testing"

	"go.viam.com/test"

	graftv1 "github.com/edaniels/graft/gen/proto/graft/v1"
)

func TestVersionString(t *testing.T) {
	tests := []struct {
		name string
		info *graftv1.VersionInfo
		want string
	}{
		{
			"tagged version",
			&graftv1.VersionInfo{Version: new("v1.2.3")},
			"v1.2.3",
		},
		{
			"devel falls through to vcs revision",
			&graftv1.VersionInfo{
				Version:     new("(devel)"),
				VcsRevision: new("abc1234def5678"),
			},
			"dev-abc1234",
		},
		{
			"pseudo version falls through to vcs revision",
			&graftv1.VersionInfo{
				Version:     new("v0.0.0-20250101000000-abc1234def56"),
				VcsRevision: new("abc1234def5678"),
			},
			"dev-abc1234",
		},
		{
			"empty version falls through to vcs revision",
			&graftv1.VersionInfo{
				Version:     new(""),
				VcsRevision: new("abc1234def5678"),
			},
			"dev-abc1234",
		},
		{
			"long vcs revision truncated to 7",
			&graftv1.VersionInfo{VcsRevision: new("abc1234def5678")},
			"dev-abc1234",
		},
		{
			"short vcs revision used as-is",
			&graftv1.VersionInfo{VcsRevision: new("abc12")},
			"dev-abc12",
		},
		{
			"exactly 7 char revision",
			&graftv1.VersionInfo{VcsRevision: new("abc1234")},
			"dev-abc1234",
		},
		{
			"dirty suffix appended",
			&graftv1.VersionInfo{
				VcsRevision: new("abc1234def5678"),
				VcsModified: new(true),
			},
			"dev-abc1234-dirty",
		},
		{
			"not dirty no suffix",
			&graftv1.VersionInfo{
				VcsRevision: new("abc1234def5678"),
				VcsModified: new(false),
			},
			"dev-abc1234",
		},
		{
			"notes only",
			&graftv1.VersionInfo{Notes: new("some note")},
			"some note",
		},
		{
			"nothing set",
			&graftv1.VersionInfo{},
			"unknown",
		},
		{
			"tagged version takes priority over vcs revision",
			&graftv1.VersionInfo{
				Version:     new("v2.0.0"),
				VcsRevision: new("abc1234"),
			},
			"v2.0.0",
		},
		{
			"vcs revision takes priority over notes",
			&graftv1.VersionInfo{
				VcsRevision: new("abc1234"),
				Notes:       new("some note"),
			},
			"dev-abc1234",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			test.That(t, versionString(tc.info), test.ShouldEqual, tc.want)
		})
	}
}

func TestBuildVersionsEqual(t *testing.T) {
	tests := []struct {
		name  string
		infoA *graftv1.VersionInfo
		infoB *graftv1.VersionInfo
		want  string
	}{
		{
			"both versions equal non-devel",
			&graftv1.VersionInfo{Version: new("v1.0.0")},
			&graftv1.VersionInfo{Version: new("v1.0.0")},
			"",
		},
		{
			"both versions devel",
			&graftv1.VersionInfo{Version: new("(devel)")},
			&graftv1.VersionInfo{Version: new("(devel)")},
			"development build",
		},
		{
			"versions differ",
			&graftv1.VersionInfo{Version: new("v1.0.0")},
			&graftv1.VersionInfo{Version: new("v2.0.0")},
			"different versions",
		},
		{
			"infoA dirty",
			&graftv1.VersionInfo{VcsModified: new(true)},
			&graftv1.VersionInfo{},
			"dirty vcs build",
		},
		{
			"infoB dirty",
			&graftv1.VersionInfo{},
			&graftv1.VersionInfo{VcsModified: new(true)},
			"dirty vcs build",
		},
		{
			"both dirty",
			&graftv1.VersionInfo{VcsModified: new(true)},
			&graftv1.VersionInfo{VcsModified: new(true)},
			"dirty vcs build",
		},
		{
			"neither dirty explicit false",
			&graftv1.VersionInfo{VcsModified: new(false)},
			&graftv1.VersionInfo{VcsModified: new(false)},
			"",
		},
		{
			"same vcs revision equal",
			&graftv1.VersionInfo{VcsRevision: new("abc1234")},
			&graftv1.VersionInfo{VcsRevision: new("abc1234")},
			"",
		},
		{
			"different vcs revisions",
			&graftv1.VersionInfo{VcsRevision: new("abc1234")},
			&graftv1.VersionInfo{VcsRevision: new("def5678")},
			"different vcs revisions",
		},
		{
			"no fields set equal",
			&graftv1.VersionInfo{},
			&graftv1.VersionInfo{},
			"",
		},
		{
			"only one side has vcs revision",
			&graftv1.VersionInfo{VcsRevision: new("abc1234")},
			&graftv1.VersionInfo{},
			"version info mismatch",
		},
		{
			"only one side has version",
			&graftv1.VersionInfo{Version: new("v1.0.0")},
			&graftv1.VersionInfo{},
			"version info mismatch",
		},
		{
			"devel version with different revisions not dirty",
			&graftv1.VersionInfo{
				Version:     new("(devel)"),
				VcsRevision: new("abc1234"),
			},
			&graftv1.VersionInfo{
				Version:     new("(devel)"),
				VcsRevision: new("def5678"),
			},
			"development build",
		},
		{
			"different versions same revision not dirty",
			&graftv1.VersionInfo{
				Version:     new("v1.0.0"),
				VcsRevision: new("abc1234"),
			},
			&graftv1.VersionInfo{
				Version:     new("v2.0.0"),
				VcsRevision: new("abc1234"),
			},
			"different versions",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			test.That(t, BuildVersionsEqual(tc.infoA, tc.infoB), test.ShouldEqual, tc.want)
		})
	}
}

func TestVersionIsInSourceTree(t *testing.T) {
	// When running tests via `go test`, build info reports (devel).
	test.That(t, VersionIsInSourceTree(), test.ShouldBeTrue)
}

func TestVersionBinaryOutput(t *testing.T) {
	// Build the binary from source.
	binPath := t.TempDir() + "/graft"

	build := exec.CommandContext(t.Context(), "go", "build", "-o", binPath, "./cmd/graft")
	build.Dir = ".."
	buildOut, err := build.CombinedOutput()
	_ = buildOut

	test.That(t, err, test.ShouldBeNil)

	// Run graft --version; urfave/cli prints "graft version <ver>\n" to stdout.
	out, err := exec.CommandContext(t.Context(), binPath, "--version").CombinedOutput()
	test.That(t, err, test.ShouldBeNil)

	line := strings.TrimSpace(string(out))
	test.That(t, line, test.ShouldContainSubstring, "version")

	ver := strings.TrimSpace(strings.SplitN(line, "version", 2)[1])

	// versionString() has two paths depending on whether `go build` embeds a real
	// tagged version or "(devel)":
	//   - Tagged build:   "v0.1.4" or "v0.1.4+dirty"  (version returned as-is)
	//   - Untagged build: "dev-<7-char-hash>[-dirty]"  (VCS revision fallback)
	isDevBuild := strings.HasPrefix(ver, "dev-")
	if !isDevBuild {
		test.That(t, ver, test.ShouldStartWith, "v")
	}

	if _, lookupErr := exec.LookPath("git"); lookupErr != nil {
		t.Log("git not available, skipping commit hash and dirty assertions")

		return
	}

	revOut, err := exec.CommandContext(t.Context(), "git", "rev-parse", "HEAD").Output()
	test.That(t, err, test.ShouldBeNil)

	rev := strings.TrimSpace(string(revOut))
	if isDevBuild {
		if len(rev) > 7 {
			rev = rev[:7]
		}

		test.That(t, ver, test.ShouldContainSubstring, rev)
	}

	// Check dirty state matches.
	diffOut, diffErr := exec.CommandContext(t.Context(), "git", "status", "--porcelain").Output()
	_ = diffErr
	dirty := len(strings.TrimSpace(string(diffOut))) > 0

	dirtySuffix := "+dirty"
	if isDevBuild {
		dirtySuffix = "-dirty"
	}

	if dirty {
		test.That(t, ver, test.ShouldEndWith, dirtySuffix)
	} else {
		test.That(t, ver, test.ShouldNotEndWith, dirtySuffix)
	}
}
