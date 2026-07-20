package graft

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"

	"github.com/denormal/go-gitignore"
)

// parseGitignoreToMutagenIgnores reads dir's .gitignore and converts its
// patterns into Mutagen-syntax ignore patterns. Returns nil when no
// .gitignore exists or it cannot be read.
//
// Git and Mutagen ignore syntaxes are close but not identical: Mutagen's
// doublestar matching lets a trailing "/**" match the directory itself
// (i.e. zero further components), while git's trailing "/**" matches only
// the directory's contents. Left untranslated, a pattern like "dir/**"
// ignores "dir" itself, Mutagen then never descends into it, and negated
// re-includes (e.g. "!dir/*.proto") inside can never apply; files git
// would sync silently vanish from the remote. See
// gitignorePatternToMutagen for the rewrite that compensates.
//
// Ignores only take effect when a sync session is created (mutagen has no
// reconfigure API); EstablishSynchronization terminates and recreates a
// session when its recorded ignores no longer match the patterns returned
// here.
func parseGitignoreToMutagenIgnores(dir string) []string {
	rd, err := os.ReadFile(filepath.Join(dir, ".gitignore"))
	if err != nil {
		return nil
	}

	parser := gitignore.NewParser(bytes.NewReader(rd), func(_ gitignore.Error) bool {
		return true
	})

	patterns := parser.Parse()

	ignores := make([]string, 0, len(patterns))
	for _, pattern := range patterns {
		ignores = append(ignores, gitignorePatternToMutagen(pattern.String()))
	}

	return ignores
}

// gitignorePatternToMutagen rewrites a single gitignore pattern into a
// Mutagen-syntax pattern with equivalent matching behavior. A trailing
// "/**" (or its directory-only form "/**/") is rewritten to "/**/*"
// ("/**/*/") so that it matches only the directory's contents, as in git,
// rather than the directory itself. Leading "**/" and interior "/**/"
// already behave the same under both syntaxes and pass through untouched,
// as do negation ("!") and anchor ("/") prefixes.
func gitignorePatternToMutagen(pattern string) string {
	switch {
	case strings.HasSuffix(pattern, "/**"):
		return pattern + "/*"
	case strings.HasSuffix(pattern, "/**/"):
		return pattern + "*/"
	}

	return pattern
}
