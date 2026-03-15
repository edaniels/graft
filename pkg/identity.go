package graft

import (
	"crypto/rand"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"strings"

	"github.com/edaniels/graft/errors"
)

// generateDaemonIdentity creates a new random identity string in the format
// "adjective-noun-verb" (e.g., "bright-falcon-soar").
func generateDaemonIdentity() string {
	adj := identityAdjectives[cryptoRandInt(len(identityAdjectives))]
	noun := identityNouns[cryptoRandInt(len(identityNouns))]
	verb := identityVerbs[cryptoRandInt(len(identityVerbs))]

	return fmt.Sprintf("%s-%s-%s", adj, noun, verb)
}

func cryptoRandInt(bound int) int {
	n, err := rand.Int(rand.Reader, big.NewInt(int64(bound)))
	if err != nil {
		panic(fmt.Sprintf("failed to generate random int: %v", err))
	}

	return int(n.Int64())
}

// daemonIdentityFromPath reads a daemon identity from the given file path.
// If the file doesn't exist, a new identity is generated and written.
func daemonIdentityFromPath(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err == nil {
		identity := strings.TrimSpace(string(data))
		if identity != "" {
			return identity, nil
		}
	}

	if !errors.Is(err, os.ErrNotExist) && err != nil {
		return "", errors.WrapPrefix(err, "error reading identity file")
	}

	identity := generateDaemonIdentity()

	if err := os.MkdirAll(filepath.Dir(path), DirPerms); err != nil {
		return "", errors.WrapPrefix(err, "error creating identity directory")
	}

	if err := os.WriteFile(path, []byte(identity), FilePerms); err != nil {
		return "", errors.WrapPrefix(err, "error writing identity file")
	}

	return identity, nil
}

// DaemonIdentity returns the local daemon's persistent identity.
// The identity is stored at ~/.local/state/graft/local/identity.
func DaemonIdentity() (string, error) {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return "", errors.Wrap(err)
	}

	stateHome := graftStateHome(homeDir)

	roleDir, err := roleSubdir(ServerRoleLocal)
	if err != nil {
		return "", err
	}

	return daemonIdentityFromPath(filepath.Join(stateHome, roleDir, "identity"))
}

var identityAdjectives = []string{
	"amber", "azure", "bold", "brave", "bright", "calm", "clear", "cool", "crisp", "deft",
	"eager", "fair", "fast", "firm", "fond", "glad", "gold", "grand", "green", "keen",
	"kind", "lean", "live", "neat", "next", "open", "pale", "pure", "rare", "real",
	"rich", "ruby", "safe", "sage", "slim", "soft", "sure", "tall", "tame", "tidy",
	"trim", "true", "vast", "warm", "wide", "wild", "wise", "wry", "young", "zest",
}

var identityNouns = []string{
	"acorn", "arrow", "aspen", "badge", "basin", "beach", "birch", "blade", "blaze", "bloom",
	"bower", "brace", "brand", "brave", "break", "brick", "brook", "brush", "cairn", "cedar",
	"chalk", "chase", "chest", "cliff", "cloud", "coast", "coral", "crane", "creek", "crest",
	"crown", "curve", "delta", "drift", "dunes", "eagle", "ember", "fable", "fawn", "field",
	"finch", "flame", "fleet", "forge", "frost", "glade", "gleam", "grove", "guard", "haven",
	"hawk", "heart", "hedge", "heron", "holly", "ivory", "jewel", "knoll", "larch", "latch",
	"ledge", "lotus", "maple", "marsh", "mason", "mirth", "moose", "north", "oasis", "olive",
	"orbit", "otter", "pearl", "perch", "pilot", "plume", "point", "polar", "prism", "quail",
	"raven", "ridge", "river", "robin", "rover", "scout", "shade", "shelf", "shore", "slate",
	"spark", "spire", "spoke", "stone", "storm", "swift", "thorn", "torch", "trail", "falcon",
}

var identityVerbs = []string{
	"bind", "bolt", "burn", "call", "carve", "cast", "claim", "clash", "climb", "cling",
	"coil", "cross", "crush", "curve", "dare", "dash", "delve", "dip", "dive", "draft",
	"drain", "draw", "drift", "drive", "dwell", "fade", "fling", "float", "forge", "fuse",
	"glide", "glow", "grasp", "grind", "guard", "guide", "hatch", "haul", "hoist", "hover",
	"kneel", "knock", "latch", "launch", "leap", "lend", "lift", "loom", "march", "mend",
	"merge", "mold", "nudge", "pace", "parse", "patch", "pave", "perch", "pluck", "plumb",
	"probe", "pull", "push", "rally", "reach", "reap", "renew", "ride", "rinse", "roam",
	"scale", "scour", "sculpt", "seek", "seize", "shift", "shine", "shove", "sift", "sink",
	"skate", "slash", "slide", "soar", "span", "spark", "split", "steer", "stoke", "sweep",
	"swell", "trace", "trek", "vault", "wade", "weave", "weld", "whirl", "yield", "zoom",
}
