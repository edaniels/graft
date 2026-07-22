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

func TestApplySyncIncludes(t *testing.T) {
	t.Run("no includes returns input unchanged", func(t *testing.T) {
		in := []string{"*.log"}
		test.That(t, applySyncIncludes(in, nil), test.ShouldResemble, in)
	})

	t.Run("appends negations translated to mutagen syntax", func(t *testing.T) {
		got := applySyncIncludes([]string{"*_pb2.py", "gen/**/*"}, []string{"**/*_pb2.py", "gen/**"})
		test.That(t, got, test.ShouldResemble, []string{
			"*_pb2.py", "gen/**/*", "!**/*_pb2.py", "!gen/**/*",
		})
	})

	t.Run("strips a redundant leading bang so includes never double-negate", func(t *testing.T) {
		test.That(t, applySyncIncludes(nil, []string{"!keep.txt"}), test.ShouldResemble, []string{"!keep.txt"})
	})

	t.Run("skips empty entries", func(t *testing.T) {
		test.That(t, applySyncIncludes([]string{"a"}, []string{"", "b"}), test.ShouldResemble, []string{"a", "!b"})
	})

	t.Run("skips a bare bang that would be a negated-empty pattern", func(t *testing.T) {
		test.That(t, applySyncIncludes([]string{"a"}, []string{"!", "b"}), test.ShouldResemble, []string{"a", "!b"})
	})

	t.Run("does not mutate the input slice", func(t *testing.T) {
		in := []string{"*.log"}
		applySyncIncludes(in, []string{"keep.txt"})
		test.That(t, in, test.ShouldResemble, []string{"*.log"})
	})
}

// TestSyncIncludeReincludesGitignoredFile feeds an assembled ignore list
// through mutagen's real ignorer and asserts that a syncInclude override
// re-includes a gitignored generated file for sync while leaving unrelated
// ignores in force. This is the core promise of syncInclude: gitignored for
// git, still synced by graft.
func TestSyncIncludeReincludesGitignoredFile(t *testing.T) {
	ignores := applySyncIncludes([]string{"*_pb2.py", "*.log"}, []string{"**/*_pb2.py"})

	ignorer, err := mutagenignore.NewIgnorer(ignores)
	test.That(t, err, test.ShouldBeNil)

	for _, tc := range []struct {
		path      string
		directory bool
		ignored   bool
	}{
		// Generated protobuf re-included by the override: synced despite .gitignore.
		{"pkg/api/service_pb2.py", false, false},
		{"service_pb2.py", false, false},
		// An unrelated ignore is untouched.
		{"debug.log", false, true},
	} {
		t.Run(tc.path, func(t *testing.T) {
			status, _ := ignorer.Ignore(tc.path, tc.directory)
			test.That(t, status == ignore.IgnoreStatusIgnored, test.ShouldEqual, tc.ignored)
		})
	}
}

func TestStaticAncestorDirs(t *testing.T) {
	for _, tc := range []struct {
		in  string
		out []string
	}{
		{"gen/api/foo_pb2.py", []string{"gen", "gen/api"}},
		{"/gen/foo_pb2.py", []string{"gen"}},
		{"gen/**/*.pb.go", []string{"gen"}},
		{"gen/api/*.pb.go", []string{"gen", "gen/api"}},
		{"**/*_pb2.py", nil},
		{"keep.txt", nil},
		{"!/gen/foo_pb2.py", []string{"gen"}},
	} {
		t.Run(tc.in, func(t *testing.T) {
			test.That(t, staticAncestorDirs(tc.in), test.ShouldResemble, tc.out)
		})
	}
}

// TestShadowedSyncIncludes verifies detection of includes that cannot take
// effect because an ignore prunes an ancestor directory: Mutagen never
// descends into an ignored directory, so the negation beneath it is never
// evaluated. This mirrors git's own "cannot re-include under an excluded
// directory" rule.
func TestShadowedSyncIncludes(t *testing.T) {
	t.Run("direct child under contents-form ignore is not shadowed", func(t *testing.T) {
		// "gen" stays traversable under "gen/**/*", so a direct child re-includes.
		ignores := applySyncIncludes([]string{"gen/**/*"}, []string{"gen/foo_pb2.py"})
		test.That(t, shadowedSyncIncludes(ignores, []string{"gen/foo_pb2.py"}), test.ShouldBeEmpty)
	})

	t.Run("deeply-nested include under contents-form ignore is shadowed", func(t *testing.T) {
		// "gen/**/*" prunes the intermediate "gen/api" directory, so a file
		// two levels down can never be reached.
		ignores := applySyncIncludes([]string{"gen/**/*"}, []string{"gen/api/foo_pb2.py"})
		test.That(t, shadowedSyncIncludes(ignores, []string{"gen/api/foo_pb2.py"}),
			test.ShouldResemble, []string{"gen/api/foo_pb2.py"})
	})

	t.Run("directory-form ignore shadows the include", func(t *testing.T) {
		ignores := applySyncIncludes([]string{"/gen/"}, []string{"/gen/foo_pb2.py"})
		test.That(t, shadowedSyncIncludes(ignores, []string{"/gen/foo_pb2.py"}),
			test.ShouldResemble, []string{"/gen/foo_pb2.py"})
	})

	t.Run("leaf-glob ignore prunes nothing, so no shadow at any depth", func(t *testing.T) {
		ignores := applySyncIncludes([]string{"*_pb2.py"}, []string{"**/*_pb2.py"})
		test.That(t, shadowedSyncIncludes(ignores, []string{"**/*_pb2.py"}), test.ShouldBeEmpty)
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
