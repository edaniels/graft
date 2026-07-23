package graft

import (
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"go.viam.com/test"

	"github.com/edaniels/graft/errors"
	graftv1 "github.com/edaniels/graft/gen/proto/graft/v1"
)

var errReadFailed = errors.NewBare("read failed")

func boolFlag(v bool) *atomic.Bool {
	b := new(atomic.Bool)
	b.Store(v)

	return b
}

func lspTestArgs() lspRewriterArgs {
	return lspRewriterArgs{
		Executable:            "rust-analyzer",
		ConnectionName:        "myconn",
		ClientSupportsContent: boolFlag(true),
		Remappings: []*graftv1.PathRemapping{
			{FromPrefix: "/Users/eric/proj", ToPrefix: "/home/user/proj"},
		},
	}
}

func TestRewriteLocalRemoteURIsRewritesTargetURI(t *testing.T) {
	args := lspTestArgs()
	value := []any{
		map[string]any{
			"targetUri": "file:///home/user/proj/crates/foo/src/lib.rs",
			"targetRange": map[string]any{
				"start": map[string]any{"line": float64(1), "character": float64(0)},
			},
		},
	}

	rewriteLocalRemoteURIs(value, args, false)

	link, ok := value[0].(map[string]any)
	test.That(t, ok, test.ShouldBeTrue)
	test.That(t, link["targetUri"], test.ShouldEqual, "file:///Users/eric/proj/crates/foo/src/lib.rs")
}

func TestRewriteLocalRemoteURIsTargetURIToRemote(t *testing.T) {
	args := lspTestArgs()
	value := map[string]any{
		"targetUri": "file:///Users/eric/proj/crates/foo/src/lib.rs",
	}

	rewriteLocalRemoteURIs(value, args, true)

	test.That(t, value["targetUri"], test.ShouldEqual, "file:///home/user/proj/crates/foo/src/lib.rs")
}

func TestRewriteLocalRemoteURIsUnmappedBecomesGraftScheme(t *testing.T) {
	args := lspTestArgs()
	value := map[string]any{
		"uri":       "file:///home/user/.cargo/registry/src/foo-1.0.0/src/lib.rs",
		"targetUri": "file:///home/user/.rustup/toolchains/stable/lib/map.rs",
	}

	rewriteLocalRemoteURIs(value, args, false)

	test.That(t, value["uri"], test.ShouldEqual,
		"graft://myconn/home/user/.cargo/registry/src/foo-1.0.0/src/lib@myconn.rs")
	test.That(t, value["targetUri"], test.ShouldEqual,
		"graft://myconn/home/user/.rustup/toolchains/stable/lib/map@myconn.rs")
}

func TestRewriteLocalRemoteURIsMappedStillWinsOverGraftScheme(t *testing.T) {
	args := lspTestArgs()
	value := map[string]any{
		"uri": "file:///home/user/proj/crates/foo/src/lib.rs",
	}

	rewriteLocalRemoteURIs(value, args, false)

	test.That(t, value["uri"], test.ShouldEqual, "file:///Users/eric/proj/crates/foo/src/lib.rs")
}

func TestRewriteLocalRemoteURIsUnmappedLocalStaysPut(t *testing.T) {
	// The local to remote direction must never invent graft URIs; the remote
	// server can only ever read real files on its side.
	args := lspTestArgs()
	value := map[string]any{"uri": "file:///Users/eric/elsewhere/file.rs"}

	rewriteLocalRemoteURIs(value, args, true)

	test.That(t, value["uri"], test.ShouldEqual, "file:///Users/eric/elsewhere/file.rs")
}

func TestRewriteLocalRemoteURIsNoConnectionNameLeavesUnmapped(t *testing.T) {
	args := lspTestArgs()
	args.ConnectionName = ""
	value := map[string]any{"uri": "file:///home/user/.cargo/x.rs"}

	rewriteLocalRemoteURIs(value, args, false)

	test.That(t, value["uri"], test.ShouldEqual, "file:///home/user/.cargo/x.rs")
}

func TestRewriteLocalRemoteURIsGraftSchemeBackToRemote(t *testing.T) {
	args := lspTestArgs()
	value := map[string]any{
		"textDocument": map[string]any{
			"uri": "graft://myconn/home/user/.cargo/registry/src/foo-1.0.0/src/lib@myconn.rs",
		},
	}

	rewriteLocalRemoteURIs(value, args, true)

	textDocument, ok := value["textDocument"].(map[string]any)
	test.That(t, ok, test.ShouldBeTrue)
	test.That(t, textDocument["uri"], test.ShouldEqual,
		"file:///home/user/.cargo/registry/src/foo-1.0.0/src/lib.rs")
}

func TestRemoteReadCommand(t *testing.T) {
	nasty := "/home/x/weird $(id) [a]*?.rs"
	args, stdin, err := remoteReadCommand("myconn", nasty)
	test.That(t, err, test.ShouldBeNil)
	// The path travels only via NUL-terminated stdin; argv is fixed, so no part
	// of the path (and no shell metacharacter in it) reaches the remote shell.
	test.That(t, stdin, test.ShouldEqual, nasty+"\x00")
	test.That(t, args[0], test.ShouldEqual, "run")
	test.That(t, args[1], test.ShouldEqual, "--to")
	test.That(t, args[2], test.ShouldEqual, "myconn")

	for _, arg := range args {
		test.That(t, strings.Contains(arg, "weird"), test.ShouldBeFalse)
		test.That(t, strings.ContainsRune(arg, '$'), test.ShouldBeFalse)
	}

	// Newlines in a path are fine (NUL is the delimiter); a NUL cannot appear.
	_, _, err = remoteReadCommand("myconn", "/home/x/new\nline.rs")
	test.That(t, err, test.ShouldBeNil)

	_, _, err = remoteReadCommand("myconn", "/home/x/nul\x00.rs")
	test.That(t, err, test.ShouldNotBeNil)
}

func TestRewriteLocalRemoteURIsDocumentLinkTarget(t *testing.T) {
	args := lspTestArgs()
	value := map[string]any{
		"target": "file:///home/user/.cargo/registry/src/foo-1.0.0/README.md",
		"range":  map[string]any{},
	}

	rewriteLocalRemoteURIs(value, args, false)
	test.That(t, value["target"], test.ShouldEqual,
		"graft://myconn/home/user/.cargo/registry/src/foo-1.0.0/README@myconn.md")

	rewriteLocalRemoteURIs(value, args, true)
	test.That(t, value["target"], test.ShouldEqual,
		"file:///home/user/.cargo/registry/src/foo-1.0.0/README.md")
}

func TestIsGraftDocumentEdit(t *testing.T) {
	graftDoc := map[string]any{
		"textDocument": map[string]any{"uri": "graft://myconn/home/x@myconn.rs"},
	}
	fileDoc := map[string]any{
		"textDocument": map[string]any{"uri": "file:///Users/eric/proj/x.rs"},
	}

	for _, method := range []string{
		"textDocument/didChange", "textDocument/didSave",
		"textDocument/willSave", "textDocument/willSaveWaitUntil",
	} {
		test.That(t, isGraftDocumentEdit(method, graftDoc), test.ShouldBeTrue)
		test.That(t, isGraftDocumentEdit(method, fileDoc), test.ShouldBeFalse)
	}

	// Opening and closing read-only docs stays allowed.
	test.That(t, isGraftDocumentEdit("textDocument/didOpen", graftDoc), test.ShouldBeFalse)
	test.That(t, isGraftDocumentEdit("textDocument/didClose", graftDoc), test.ShouldBeFalse)
	test.That(t, isGraftDocumentEdit("textDocument/didChange", nil), test.ShouldBeFalse)
	test.That(t, isGraftDocumentEdit("textDocument/didChange", "bogus"), test.ShouldBeFalse)
}

func TestDecorateAndStripRemotePathSegment(t *testing.T) {
	for path, decorated := range map[string]string{
		"/a/b/context.rs":       "/a/b/context@myconn.rs",
		"/a/b/Makefile":         "/a/b/Makefile@myconn",
		"/a/b/.bashrc":          "/a/b/.bashrc@myconn",
		"/a/b/file.tar.gz":      "/a/b/file.tar@myconn.gz",
		"/a/b/x@myconn.rs":      "/a/b/x@myconn@myconn.rs",
		"/spaced dir/f i le.rs": "/spaced dir/f i le@myconn.rs",
	} {
		test.That(t, decorateRemotePathSegment(path, "myconn"), test.ShouldEqual, decorated)
		test.That(t, stripRemotePathSegment(decorated, "myconn"), test.ShouldEqual, path)
	}

	// An undecorated path passes through strip untouched.
	test.That(t, stripRemotePathSegment("/a/b/context.rs", "myconn"),
		test.ShouldEqual, "/a/b/context.rs")

	// Degenerate paths pass through both untouched.
	for _, path := range []string{"", "/", "/a/b/"} {
		test.That(t, decorateRemotePathSegment(path, "myconn"), test.ShouldEqual, path)
		test.That(t, stripRemotePathSegment(path, "myconn"), test.ShouldEqual, path)
	}
}

func TestRewriteLocalRemoteURIsGraftSchemeOtherConnectionUntouched(t *testing.T) {
	args := lspTestArgs()
	value := map[string]any{"uri": "graft://other-conn/home/x.rs"}

	rewriteLocalRemoteURIs(value, args, true)

	test.That(t, value["uri"], test.ShouldEqual, "graft://other-conn/home/x.rs")
}

func TestRewriteLocalRemoteURIsLocallyExistingFileNotMinted(t *testing.T) {
	// A URI whose path exists on the local machine is one the client can read
	// directly; minting a graft URI for it would break clients whose own
	// unmapped URIs get echoed back by the server (e.g. publishDiagnostics for
	// a file opened outside the sync root).
	args := lspTestArgs()
	localFile := filepath.Join(t.TempDir(), "file.rs")
	test.That(t, os.WriteFile(localFile, []byte("fn main() {}"), 0o600), test.ShouldBeNil)

	value := map[string]any{"uri": "file://" + localFile}

	rewriteLocalRemoteURIs(value, args, false)

	test.That(t, value["uri"], test.ShouldEqual, "file://"+localFile)
}

func TestRewriteLocalRemoteURIsClientWithoutContentSupportNoMint(t *testing.T) {
	// Clients that never declared workspace.textDocumentContent cannot open
	// graft URIs at all; the raw remote path is no worse and sometimes works
	// (identical layouts on both sides).
	for _, flag := range []*atomic.Bool{nil, boolFlag(false)} {
		args := lspTestArgs()
		args.ClientSupportsContent = flag
		value := map[string]any{"uri": "file:///home/user/.cargo/x.rs"}

		rewriteLocalRemoteURIs(value, args, false)

		test.That(t, value["uri"], test.ShouldEqual, "file:///home/user/.cargo/x.rs")
	}
}

func TestRewriteLocalRemoteURIsMintPreservesQueryAndFragment(t *testing.T) {
	args := lspTestArgs()
	value := map[string]any{"uri": "file:///home/user/.cargo/x.rs?q=1#L10"}

	rewriteLocalRemoteURIs(value, args, false)
	test.That(t, value["uri"], test.ShouldEqual, "graft://myconn/home/user/.cargo/x@myconn.rs?q=1#L10")

	rewriteLocalRemoteURIs(value, args, true)
	test.That(t, value["uri"], test.ShouldEqual, "file:///home/user/.cargo/x.rs?q=1#L10")
}

func TestRewriteLocalRemoteURIsPrefixBoundary(t *testing.T) {
	// /home/user/projfoo must not match the /home/user/proj
	// remapping prefix; it is a remote-only file and should be minted instead
	// of misrewritten to /Users/eric/projfoo.
	args := lspTestArgs()
	value := map[string]any{"uri": "file:///home/user/projfoo/src/x.rs"}

	rewriteLocalRemoteURIs(value, args, false)

	test.That(t, value["uri"], test.ShouldEqual, "graft://myconn/home/user/projfoo/src/x@myconn.rs")

	// Same boundary rule on the way out.
	value = map[string]any{"uri": "file:///Users/eric/projfoo/src/x.rs"}

	rewriteLocalRemoteURIs(value, args, true)

	test.That(t, value["uri"], test.ShouldEqual, "file:///Users/eric/projfoo/src/x.rs")
}

func TestConnectionNameIsURIHostSafe(t *testing.T) {
	for name, want := range map[string]bool{
		"myconn":  true,
		"MyConn":  true, // case variant round trips (host matching is case-insensitive)
		"conn_1":  true,
		"my.host": true,
		"a:1":     true, // parses as host:port but round trips intact
		"my conn": false,
		"a/b":     false,
		"a:b":     false, // invalid port; does not survive url.Parse
		"":        false,
	} {
		test.That(t, connectionNameIsURIHostSafe(name), test.ShouldEqual, want)
	}
}

func TestClientDeclaresTextDocumentContent(t *testing.T) {
	declared := map[string]any{
		"capabilities": map[string]any{
			"workspace": map[string]any{
				"textDocumentContent": map[string]any{"dynamicRegistration": true},
			},
		},
	}
	test.That(t, clientDeclaresTextDocumentContent(declared), test.ShouldBeTrue)

	for _, params := range []any{
		nil,
		"bogus",
		map[string]any{},
		map[string]any{"capabilities": map[string]any{}},
		map[string]any{"capabilities": map[string]any{"workspace": map[string]any{}}},
	} {
		test.That(t, clientDeclaresTextDocumentContent(params), test.ShouldBeFalse)
	}
}

func TestParamsURIScheme(t *testing.T) {
	test.That(t, paramsURIScheme(map[string]any{"uri": "graft://myconn/x.rs"}), test.ShouldEqual, "graft")
	test.That(t, paramsURIScheme(map[string]any{"uri": "future://x"}), test.ShouldEqual, "future")
	test.That(t, paramsURIScheme(map[string]any{}), test.ShouldEqual, "")
	test.That(t, paramsURIScheme(nil), test.ShouldEqual, "")
}

func TestInjectTextDocumentContentCapability(t *testing.T) {
	result := map[string]any{
		"capabilities": map[string]any{
			"hoverProvider": true,
			"workspace": map[string]any{
				"workspaceFolders": map[string]any{"supported": true},
			},
		},
	}

	injectTextDocumentContentCapability(result)

	caps, ok := result["capabilities"].(map[string]any)
	test.That(t, ok, test.ShouldBeTrue)

	// Flat name read by Sublime Text's LSP package (v2.13.0).
	provider, ok := caps["textDocumentContentProvider"].(map[string]any)
	test.That(t, ok, test.ShouldBeTrue)
	test.That(t, provider["schemes"], test.ShouldResemble, []any{"graft"})

	// Spec (LSP 3.18) name, merged into the existing workspace capabilities.
	workspace, ok := caps["workspace"].(map[string]any)
	test.That(t, ok, test.ShouldBeTrue)
	content, ok := workspace["textDocumentContent"].(map[string]any)
	test.That(t, ok, test.ShouldBeTrue)
	test.That(t, content["schemes"], test.ShouldResemble, []any{"graft"})

	test.That(t, workspace["workspaceFolders"], test.ShouldNotBeNil)
	test.That(t, caps["hoverProvider"], test.ShouldEqual, true)
}

func TestInjectTextDocumentContentCapabilityMergesServerSchemes(t *testing.T) {
	// A server with native textDocumentContent support keeps its own schemes.
	result := map[string]any{
		"capabilities": map[string]any{
			"workspace": map[string]any{
				"textDocumentContent": map[string]any{"schemes": []any{"rust-macro"}},
			},
		},
	}

	injectTextDocumentContentCapability(result)

	caps, ok := result["capabilities"].(map[string]any)
	test.That(t, ok, test.ShouldBeTrue)
	workspace, ok := caps["workspace"].(map[string]any)
	test.That(t, ok, test.ShouldBeTrue)
	content, ok := workspace["textDocumentContent"].(map[string]any)
	test.That(t, ok, test.ShouldBeTrue)
	test.That(t, content["schemes"], test.ShouldResemble, []any{"rust-macro", "graft"})

	provider, ok := caps["textDocumentContentProvider"].(map[string]any)
	test.That(t, ok, test.ShouldBeTrue)
	test.That(t, provider["schemes"], test.ShouldResemble, []any{"rust-macro", "graft"})
}

func TestInjectTextDocumentContentCapabilityNoWorkspace(t *testing.T) {
	result := map[string]any{"capabilities": map[string]any{}}

	injectTextDocumentContentCapability(result)

	caps, ok := result["capabilities"].(map[string]any)
	test.That(t, ok, test.ShouldBeTrue)
	workspace, ok := caps["workspace"].(map[string]any)
	test.That(t, ok, test.ShouldBeTrue)
	content, ok := workspace["textDocumentContent"].(map[string]any)
	test.That(t, ok, test.ShouldBeTrue)
	test.That(t, content["schemes"], test.ShouldResemble, []any{"graft"})
}

func TestInjectTextDocumentContentCapabilityNonMapValues(t *testing.T) {
	// Must not panic on shapes we do not expect and must leave them alone.
	injectTextDocumentContentCapability(nil)
	injectTextDocumentContentCapability("bogus")

	result := map[string]any{"capabilities": "bogus"}
	injectTextDocumentContentCapability(result)
	test.That(t, result["capabilities"], test.ShouldEqual, "bogus")
}

func TestHandleTextDocumentContent(t *testing.T) {
	args := lspTestArgs()

	var gotPath string

	readFile := func(path string) (string, error) {
		gotPath = path

		return "fn main() {}", nil
	}

	result, err := handleTextDocumentContent(map[string]any{
		"uri": "graft://myconn/home/user/.cargo/registry/src/foo-1.0.0/src/lib@myconn.rs",
	}, args, readFile)
	test.That(t, err, test.ShouldBeNil)
	test.That(t, gotPath, test.ShouldEqual, "/home/user/.cargo/registry/src/foo-1.0.0/src/lib.rs")
	test.That(t, result, test.ShouldResemble, map[string]any{"text": "fn main() {}"})
}

func TestHandleTextDocumentContentRejectsNonGraftScheme(t *testing.T) {
	args := lspTestArgs()
	readFile := func(string) (string, error) { return "", nil }

	_, err := handleTextDocumentContent(map[string]any{
		"uri": "file:///home/user/.cargo/x.rs",
	}, args, readFile)
	test.That(t, err, test.ShouldNotBeNil)
}

func TestHandleTextDocumentContentRejectsOtherConnection(t *testing.T) {
	args := lspTestArgs()
	readFile := func(string) (string, error) { return "", nil }

	_, err := handleTextDocumentContent(map[string]any{
		"uri": "graft://other-conn/home/user/.cargo/x.rs",
	}, args, readFile)
	test.That(t, err, test.ShouldNotBeNil)
}

func TestHandleTextDocumentContentReadErrorPropagates(t *testing.T) {
	args := lspTestArgs()
	readFile := func(string) (string, error) { return "", errors.Wrap(errReadFailed) }

	_, err := handleTextDocumentContent(map[string]any{
		"uri": "graft://myconn/home/user/.cargo/x.rs",
	}, args, readFile)
	test.That(t, err, test.ShouldNotBeNil)
	test.That(t, err.Error(), test.ShouldContainSubstring, errReadFailed.Error())
}

func TestHandleTextDocumentContentBadParams(t *testing.T) {
	args := lspTestArgs()
	readFile := func(string) (string, error) { return "", nil }

	_, err := handleTextDocumentContent("bogus", args, readFile)
	test.That(t, err, test.ShouldNotBeNil)

	_, err = handleTextDocumentContent(map[string]any{}, args, readFile)
	test.That(t, err, test.ShouldNotBeNil)
}
