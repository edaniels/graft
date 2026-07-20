package graft

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/mutagen-io/mutagen/pkg/synchronization/core/ignore"
	mutagenignore "github.com/mutagen-io/mutagen/pkg/synchronization/core/ignore/mutagen"
	"go.viam.com/test"
)

func TestGitignorePatternToMutagen(t *testing.T) {
	for _, tc := range []struct {
		in, out string
	}{
		{"/server/**/*proto/**", "/server/**/*proto/**/*"},
		{"!/server/**/*proto/**", "!/server/**/*proto/**/*"},
		{"build/**/", "build/**/*/"},
		{"node_modules", "node_modules"},
		{"*.pb.go", "*.pb.go"},
		{"/target", "/target"},
		{"**/build/", "**/build/"},
		{"/go/**/bin/", "/go/**/bin/"},
		{"!/client/py/modal_proto/__init__.py", "!/client/py/modal_proto/__init__.py"},
	} {
		t.Run(tc.in, func(t *testing.T) {
			test.That(t, gitignorePatternToMutagen(tc.in), test.ShouldEqual, tc.out)
		})
	}
}

func TestParseGitignoreToMutagenIgnores(t *testing.T) {
	t.Run("missing file", func(t *testing.T) {
		test.That(t, parseGitignoreToMutagenIgnores(t.TempDir()), test.ShouldBeNil)
	})

	t.Run("patterns parsed and translated", func(t *testing.T) {
		dir := t.TempDir()
		gitignore := `# generated protobufs
/server/**/*proto/**
!/server/**/*proto/*.proto

*.log
`
		test.That(t, os.WriteFile(filepath.Join(dir, ".gitignore"), []byte(gitignore), 0o600), test.ShouldBeNil)

		ignores := parseGitignoreToMutagenIgnores(dir)
		test.That(t, ignores, test.ShouldResemble, []string{
			"/server/**/*proto/**/*",
			"!/server/**/*proto/*.proto",
			"*.log",
		})
	})
}

// TestGitignoreTranslationMatchesGitSemantics feeds translated patterns
// through mutagen's actual ignorer and asserts git-equivalent decisions.
// The patterns are the "generated protobufs" block of modal's .gitignore:
// untranslated, mutagen ignores the *proto directories themselves, never
// descends into them, and the negated re-includes can never apply.
func TestGitignoreTranslationMatchesGitSemantics(t *testing.T) {
	gitignorePatterns := []string{
		"/server/**/*proto/**",
		"!/server/**/*proto/*.proto",
		"!/server/**/*proto/BUILD.bazel",
	}

	translated := make([]string, len(gitignorePatterns))
	for i, p := range gitignorePatterns {
		translated[i] = gitignorePatternToMutagen(p)
	}

	ignorer, err := mutagenignore.NewIgnorer(translated)
	test.That(t, err, test.ShouldBeNil)

	for _, tc := range []struct {
		path      string
		directory bool
		ignored   bool
	}{
		// The directory itself must not be ignored, or mutagen prunes the
		// subtree before the re-includes are consulted.
		{"server/modal_server/proto", true, false},
		// Sources re-included by the negations.
		{"server/modal_server/proto/internal_api.proto", false, false},
		{"server/modal_server/proto/BUILD.bazel", false, false},
		// Generated outputs stay ignored.
		{"server/modal_server/proto/internal_api_pb2.py", false, true},
		{"server/modal_server/proto/generated", true, true},
		// "*proto" matches neither the "protos" directory nor a "*.proto"
		// leaf file, so nothing under this tree is ignored (matches git).
		{"server/modal_server/authzed/protos", true, false},
		{"server/modal_server/authzed/protos/BUILD.bazel", false, false},
		{"server/modal_server/authzed/protos/authzed/api/v1", true, false},
		{"server/modal_server/authzed/protos/authzed/api/v1/core.proto", false, false},
	} {
		t.Run(tc.path, func(t *testing.T) {
			status, _ := ignorer.Ignore(tc.path, tc.directory)
			test.That(t, status == ignore.IgnoreStatusIgnored, test.ShouldEqual, tc.ignored)
		})
	}
}
