package graft

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/denormal/go-gitignore"
	"github.com/mutagen-io/mutagen/pkg/synchronization/core/ignore"
	mutagenignore "github.com/mutagen-io/mutagen/pkg/synchronization/core/ignore/mutagen"
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

// applySyncIncludes appends graft's syncInclude override patterns to the
// gitignore-derived Mutagen ignore list. Each include is translated to
// Mutagen syntax and appended as a trailing "!" negation so it wins under
// Mutagen's last-match-wins evaluation, re-including files the .gitignore
// (and thus the sync) would otherwise skip. This is how a path stays
// gitignored for git yet still synced by graft.
//
// A negation only re-includes content whose parent directories the scan still
// traverses: Mutagen (like git) prunes any directory it ignores outright and
// never descends to evaluate negations beneath it. Includes therefore take
// effect against contents-form ignores ("dir/**") and leaf globs ("*_pb2.py"),
// but not against a whole-directory ignore ("dir/"); see shadowedSyncIncludes,
// which flags the latter so it never fails silently.
func applySyncIncludes(ignores, includes []string) []string {
	if len(includes) == 0 {
		return ignores
	}

	out := slices.Clone(ignores)

	for _, inc := range includes {
		// syncInclude entries are include patterns, not raw ignore syntax; a
		// leading "!" would double-negate into a plain ignore, so strip it.
		inc = strings.TrimPrefix(inc, "!")

		// Skip empties (including a bare "!"): "!" alone is a negated-empty
		// pattern mutagen rejects, which would fail session creation opaquely.
		if inc == "" {
			continue
		}

		out = append(out, gitignorePatternToMutagen("!"+inc))
	}

	return out
}

// shadowedSyncIncludes returns the subset of includes that cannot take effect
// against the assembled ignore list because a whole-directory ignore prunes
// one of their static ancestor directories. Mutagen (like git) never descends
// into an ignored directory, so a "!dir/leaf" negation beneath it is never
// evaluated; the file silently fails to sync. Callers warn on the result and
// advise ignoring the directory's contents ("dir/**") instead.
//
// Detection is best-effort: it only reasons about the glob-free leading path
// of each include (see staticAncestorDirs), so an include with no static
// prefix (e.g. "**/*_pb2.py") is never flagged even if a deep ignored
// directory would shadow it. That case is documented, not detected.
func shadowedSyncIncludes(ignores, includes []string) []string {
	if len(includes) == 0 {
		return nil
	}

	ignorer, err := mutagenignore.NewIgnorer(ignores)
	if err != nil {
		// A malformed list surfaces as a session-creation error elsewhere;
		// don't second-guess it here.
		return nil
	}

	var shadowed []string

	for _, inc := range includes {
		for _, dir := range staticAncestorDirs(inc) {
			if status, _ := ignorer.Ignore(dir, true); status == ignore.IgnoreStatusIgnored {
				shadowed = append(shadowed, inc)

				break
			}
		}
	}

	return shadowed
}

// staticAncestorDirs returns the glob-free ancestor directory paths that must
// be traversed to reach the content an include pattern targets. It stops at
// the first path segment containing a glob metacharacter; a pattern whose
// first segment is a glob (e.g. "**/*_pb2.py") has no static ancestors. A
// leading "!" and anchoring "/" are ignored, as is a trailing "/".
func staticAncestorDirs(pattern string) []string {
	p := strings.TrimPrefix(pattern, "!")
	p = strings.TrimPrefix(p, "/")
	p = strings.TrimSuffix(p, "/")

	if p == "" {
		return nil
	}

	segs := strings.Split(p, "/")

	limit := len(segs)

	for i, s := range segs {
		if strings.ContainsAny(s, "*?[{") {
			limit = i

			break
		}
	}

	// With no glob segment the final segment is the target content itself, so
	// its ancestors stop one short.
	if limit == len(segs) {
		limit--
	}

	var dirs []string
	for i := 1; i <= limit; i++ {
		dirs = append(dirs, strings.Join(segs[:i], "/"))
	}

	return dirs
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
