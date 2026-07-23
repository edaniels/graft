package graft

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"

	"golang.org/x/exp/jsonrpc2"

	"github.com/edaniels/graft/errors"
	graftv1 "github.com/edaniels/graft/gen/proto/graft/v1"
)

var (
	clientRequestsWaiting atomic.Int32
	serverRequestsWaiting atomic.Int32
)

// URI scheme, method, and capability names for serving remote-only file content.
// Files outside every path remapping (e.g. cargo registry or toolchain sources)
// that do not exist locally are exposed as graft://<connection>/<path> URIs whose
// content is served over the connection via the LSP 3.18
// workspace/textDocumentContent request instead of the local filesystem.
const (
	graftURIScheme                = "graft"
	fileURIScheme                 = "file"
	methodInitialize              = "initialize"
	methodTextDocumentContent     = "workspace/textDocumentContent"
	capTextDocumentContentFlat    = "textDocumentContentProvider"
	capTextDocumentContentNested  = "textDocumentContent"
	capTextDocumentContentSchemes = "schemes"
)

// ServeLSP serves a stdio LSP server forwarding responses from the session's connection.
// If the executable is not available on the remote (or there is no connection), it falls back
// to executing the local binary directly.
func (client *LocalClient) ServeLSP(ctx context.Context, executable string) error {
	client.logger.InfoContext(ctx, "serving LSP", "executable", executable)

	selectResp, err := client.selectConnection(ctx)
	if err != nil {
		fmt.Fprintln(os.Stderr, "failed to select connection; running locally")

		return ExecLocalLSP(executable)
	}

	availResp, err := client.GetConnectionAvailableCommands(ctx,
		&graftv1.GetConnectionAvailableCommandsRequest{
			Pid:            client.ppid,
			Cwd:            client.cwd,
			ConnectionName: selectResp.GetConnectionName(),
		})
	if err != nil || !slices.ContainsFunc(availResp.GetCommands(), func(cmd string) bool {
		return filepath.Base(cmd) == executable
	}) {
		fmt.Fprintln(os.Stderr, client.cwd, selectResp.GetConnectionName(), availResp.GetCommands())
		fmt.Fprintln(os.Stderr, "failed to find remote lsp; running locally")

		return ExecLocalLSP(executable)
	}

	fmt.Fprintln(os.Stderr, "will run remote lsp")

	var lspArgs lspRewriterArgs

	lspArgs.Executable = executable
	lspArgs.ConnectionName = selectResp.GetConnectionName()
	lspArgs.ClientSupportsContent = new(atomic.Bool)
	lspArgs.Remappings = selectResp.GetPathRemappings()

	if !connectionNameIsURIHostSafe(lspArgs.ConnectionName) {
		// Without a URI-host-safe name we cannot mint graft URIs that round
		// trip; path remapping still works, remote-only files just pass through.
		fmt.Fprintf(os.Stderr,
			"connection name %q cannot be used in URIs; remote-only files will not be served\n",
			lspArgs.ConnectionName)

		lspArgs.ConnectionName = ""
	}

	capIn := io.Discard
	capOut := io.Discard

	binder := &lspBinder{
		args: lspArgs,
	}
	defer binder.bindings.Wait()

	server, err := jsonrpc2.Serve(ctx, &stdioPipeListener{
		in:        io.TeeReader(os.Stdin, capIn),
		out:       io.MultiWriter(client.outWriter, capOut),
		outCloser: client.outWriter.Close,
	}, binder)
	if err != nil {
		return client.handleError(err)
	}

	if err := server.Wait(); err != nil {
		return client.handleError(err)
	}

	return nil
}

type readerWriterPipe struct {
	reader io.ReadCloser
	writer io.Writer
	closer func()
}

func (p readerWriterPipe) Read(data []byte) (int, error) {
	n, err := p.reader.Read(data)
	if err != nil {
		return n, errors.Wrap(err)
	}

	return n, nil
}

func (p readerWriterPipe) Write(data []byte) (int, error) {
	n, err := p.writer.Write(data)
	if err != nil {
		return n, errors.Wrap(err)
	}

	return n, nil
}

func (p readerWriterPipe) Close() error {
	p.closer()

	return nil
}

type stdioPipeListener struct {
	acceptOnce sync.Once
	in         io.Reader
	out        io.Writer
	outCloser  func() error
}

func (l *stdioPipeListener) Accept(_ context.Context) (io.ReadWriteCloser, error) {
	var rwc io.ReadWriteCloser

	l.acceptOnce.Do(func() {
		inR, inW := io.Pipe()
		rwc = readerWriterPipe{
			// reader: l.in,
			reader: inR,
			writer: l.out,
			closer: func() {
				inW.Close()
				inR.Close()
				errors.Unchecked(l.outCloser())
			},
		}

		go func() {
			// transfer stdin EOF to pipe
			defer inW.Close()

			// copy until eof
			errors.UncheckedValue(io.Copy(inW, l.in))
		}()
	})

	_ = errors.Errorf

	if rwc != nil {
		return rwc, nil
	}

	return nil, io.ErrClosedPipe
}

func (l *stdioPipeListener) Dialer() jsonrpc2.Dialer {
	return nil
}

func (l *stdioPipeListener) Close() error {
	return nil
}

type lspBinder struct {
	args     lspRewriterArgs
	bindings sync.WaitGroup
}

//nolint:gocognit // TODO(erd): too early to refactor
func (l *lspBinder) Bind(ctx context.Context, conn *jsonrpc2.Connection) (jsonrpc2.ConnectionOptions, error) {
	// TODO(erd): when to close?
	bindCtx, cancelBindCtx := context.WithCancel(ctx)

	//nolint:gosec // input is trusted
	execCmd := exec.CommandContext(bindCtx, os.Args[0], "run", l.args.Executable)

	stdinPipe, err := execCmd.StdinPipe()
	if err != nil {
		cancelBindCtx()

		return jsonrpc2.ConnectionOptions{}, errors.Wrap(err)
	}

	stdoutPipe, err := execCmd.StdoutPipe()
	if err != nil {
		cancelBindCtx()

		return jsonrpc2.ConnectionOptions{}, errors.Wrap(err)
	}

	stderrPipe, err := execCmd.StderrPipe()
	if err != nil {
		cancelBindCtx()

		return jsonrpc2.ConnectionOptions{}, errors.Wrap(err)
	}

	var activeWorkers sync.WaitGroup

	activeWorkers.Go(
		func() { errors.UncheckedValue(io.Copy(os.Stderr, stderrPipe)) },
	)

	if err := execCmd.Start(); err != nil {
		cancelBindCtx()

		return jsonrpc2.ConnectionOptions{}, errors.Wrap(err)
	}

	headerFramer := jsonrpc2.HeaderFramer()

	// A frame is written as multiple pipe writes and both the reader goroutine
	// (responses to server requests) and the client handler (forwarded client
	// requests) write to the server's stdin, so whole frames must be serialized.
	var stdinMu sync.Mutex

	stdinWriter := headerFramer.Writer(stdinPipe)

	writeToServer := func(msg jsonrpc2.Message) error {
		stdinMu.Lock()
		defer stdinMu.Unlock()

		if _, err := stdinWriter.Write(bindCtx, msg); err != nil {
			return errors.Wrap(err)
		}

		return nil
	}

	// TODO(erd): trim over time
	// TODO(erd): cant we just use a jsonrpc2.Connection instead as the abstraction to the command? probably
	var inflightMu sync.Mutex

	inflightClientRequests := map[any]chan any{}

	activeWorkers.Go(func() {
		frameReader := headerFramer.Reader(stdoutPipe)

		for {
			msg, _, err := frameReader.Read(bindCtx)
			if err != nil {
				return
			}

			switch v := msg.(type) {
			case *jsonrpc2.Request:
				serverRequestsWaiting.Add(1)

				var params any
				if err := json.Unmarshal(v.Params, &params); err != nil {
					serverRequestsWaiting.Add(-1)

					errors.Unchecked(writeToServer(&jsonrpc2.Response{ID: v.ID, Error: err}))

					continue
				}

				rewriteLocalRemoteURIs(params, l.args, false)
				// TODO(erd): do rewriting?

				if !v.ID.IsValid() {
					serverRequestsWaiting.Add(-1)

					errors.Unchecked(conn.Notify(bindCtx, v.Method, params))

					continue
				}

				clientResp := conn.Call(bindCtx, v.Method, params)
				// TODO(erd): maybe do a list instead
				inflightMu.Lock()

				if bindCtx.Err() != nil {
					inflightMu.Unlock()

					return
				}

				activeWorkers.Go(func() {
					defer serverRequestsWaiting.Add(-1)

					var result any
					if err := clientResp.Await(bindCtx, &result); err != nil {
						errors.Unchecked(writeToServer(&jsonrpc2.Response{ID: v.ID, Error: err}))

						return
					}

					rewriteLocalRemoteURIs(result, l.args, true)

					md, err := json.Marshal(result)
					if err != nil {
						return
					}

					errors.Unchecked(writeToServer(&jsonrpc2.Response{ID: v.ID, Result: md}))
				})
				inflightMu.Unlock()
			case *jsonrpc2.Response:
				inflightMu.Lock()

				respCh, ok := inflightClientRequests[v.ID]
				delete(inflightClientRequests, v.ID)
				inflightMu.Unlock()

				if ok {
					if v.Error != nil {
						respCh <- v.Error

						continue
					}

					var results any
					if err := json.Unmarshal(v.Result, &results); err != nil {
						continue
					}

					rewriteLocalRemoteURIs(results, l.args, false)
					// md, err := json.Marshal(results)
					// if err != nil {
					// 	continue
					// }
					if results == nil {
						// we do this because the jsonrpc2 library gets mad with nil, nil for result, err
						results = nilJSONValue{}
					}

					respCh <- results
				}
			default:
			}
		}
	})

	// Close the connection if the command is done
	// TODO(erd): Add debug logging for LSP connection failures.
	l.bindings.Go(func() {
		errors.Unchecked(execCmd.Wait())

		inflightMu.Lock()
		cancelBindCtx()
		inflightMu.Unlock()
		activeWorkers.Wait()

		errors.Unchecked(conn.Close())
	})

	return jsonrpc2.ConnectionOptions{
		Handler: jsonrpc2.HandlerFunc(func(_ context.Context, req *jsonrpc2.Request) (any, error) {
			clientRequestsWaiting.Add(1)

			var params any
			if err := json.Unmarshal(req.Params, &params); err != nil {
				clientRequestsWaiting.Add(-1)

				return nil, errors.Wrap(err)
			}

			if req.Method == methodInitialize && l.args.ClientSupportsContent != nil &&
				clientDeclaresTextDocumentContent(params) {
				l.args.ClientSupportsContent.Store(true)
			}

			if isGraftDocumentEdit(req.Method, params) {
				clientRequestsWaiting.Add(-1)

				// Swallow edits to read-only graft documents; requests
				// (willSaveWaitUntil) get a null result, notifications nothing.
				//nolint:nilnil // deliberate null response
				return nil, nil
			}

			if req.Method == methodTextDocumentContent && paramsURIScheme(params) == graftURIScheme {
				// The language server does not know our scheme; serve the content
				// ourselves from the remote side of the connection. Other schemes
				// fall through to the server, which may natively support them.
				if !req.ID.IsValid() {
					clientRequestsWaiting.Add(-1)

					//nolint:nilnil // a notification gets no response
					return nil, nil
				}

				// Respond off the delivery goroutine so a slow transfer does not
				// stall unrelated client traffic behind it.
				go func() {
					defer clientRequestsWaiting.Add(-1)

					result, err := handleTextDocumentContent(params, l.args, func(path string) (string, error) {
						return readRemoteFile(bindCtx, l.args.ConnectionName, path)
					})
					errors.Unchecked(conn.Respond(req.ID, result, err))
				}()

				return nil, jsonrpc2.ErrAsyncResponse
			}

			rewriteLocalRemoteURIs(params, l.args, true)

			md, err := json.Marshal(params)
			if err != nil {
				clientRequestsWaiting.Add(-1)

				return nil, errors.Wrap(err)
			}

			req.Params = md

			var respCh chan any
			if req.ID.IsValid() {
				// this is a request, not a notification
				// TODO(erd): dupe detection
				respCh = make(chan any, 1) // buffered since we don't need blocking

				inflightMu.Lock()

				inflightClientRequests[req.ID] = respCh

				inflightMu.Unlock()
			}

			if err := writeToServer(req); err != nil {
				clientRequestsWaiting.Add(-1)

				if respCh != nil {
					inflightMu.Lock()
					delete(inflightClientRequests, req.ID)
					inflightMu.Unlock()
				}

				return nil, errors.Wrap(err)
			}

			if respCh == nil {
				clientRequestsWaiting.Add(-1)

				//nolint:nilnil // notification means no response or error
				return nil, nil
			}

			// Await the server's response off the delivery goroutine so one slow
			// request does not serialize every other client request behind it.
			go func() {
				defer clientRequestsWaiting.Add(-1)

				select {
				case <-bindCtx.Done():
					errors.Unchecked(conn.Respond(req.ID, nil, bindCtx.Err()))
				case resp := <-respCh:
					if err, ok := resp.(error); ok {
						errors.Unchecked(conn.Respond(req.ID, nil, errors.Wrap(err)))

						return
					}

					if req.Method == methodInitialize && l.args.ConnectionName != "" &&
						l.args.ClientSupportsContent != nil && l.args.ClientSupportsContent.Load() {
						injectTextDocumentContentCapability(resp)
					}

					errors.Unchecked(conn.Respond(req.ID, resp, nil))
				}
			}()

			return nil, jsonrpc2.ErrAsyncResponse
		}),
	}, nil
}

type nilJSONValue struct{}

func (v nilJSONValue) MarshalJSON() ([]byte, error) {
	md, err := json.Marshal(nil)
	if err != nil {
		return nil, errors.Wrap(err)
	}

	return md, nil
}

//nolint:gocognit // TODO(erd): too early to refactor
func rewriteLocalRemoteURIs(value any, args lspRewriterArgs, forRemote bool) {
	switch v := value.(type) {
	case []any:
		for _, elem := range v {
			rewriteLocalRemoteURIs(elem, args, forRemote)
		}
	case map[string]any:
		for mapK, mapV := range v {
			switch mapK {
			case "globPattern":
				vStr, ok := mapV.(string)
				if !ok {
					continue
				}

				for _, mapping := range args.Remappings {
					var from, to string
					if forRemote {
						from = mapping.GetFromPrefix()
						to = mapping.GetToPrefix()
					} else {
						from = mapping.GetToPrefix()
						to = mapping.GetFromPrefix()
					}

					cutStr, found := strings.CutPrefix(vStr, from)
					if !found || !pathBoundaryOK(cutStr) {
						continue
					}

					v[mapK] = filepath.Join(to, cutStr)

					break
				}
			case "uri", "rootUri", "scopeUri", "targetUri", "target":
				vStr, ok := mapV.(string)
				if !ok {
					continue
				}

				parsedURL, err := url.Parse(vStr)
				if err != nil {
					continue
				}

				if forRemote && parsedURL.Scheme == graftURIScheme {
					// A URI we minted for a remote file outside every remapping;
					// hand the real path back to the server.
					if !strings.EqualFold(parsedURL.Host, args.ConnectionName) {
						continue
					}

					parsedURL.Scheme = fileURIScheme
					parsedURL.Host = ""
					parsedURL.Path = stripRemotePathSegment(parsedURL.Path, args.ConnectionName)
					v[mapK] = parsedURL.String()

					continue
				}

				if parsedURL.Scheme != fileURIScheme {
					continue
				}

				var matched bool

				for _, mapping := range args.Remappings {
					var from, to string
					if forRemote {
						from = mapping.GetFromPrefix()
						to = mapping.GetToPrefix()
					} else {
						from = mapping.GetToPrefix()
						to = mapping.GetFromPrefix()
					}

					cutStr, found := strings.CutPrefix(parsedURL.Path, from)
					if !found || !pathBoundaryOK(cutStr) {
						continue
					}

					parsedURL.Path = filepath.Join(to, cutStr)
					v[mapK] = parsedURL.String()
					matched = true

					break
				}

				if !matched && !forRemote && args.ConnectionName != "" &&
					args.ClientSupportsContent != nil && args.ClientSupportsContent.Load() &&
					!localPathExists(parsedURL.Path) {
					// A remote-only file outside every synced tree (e.g. cargo
					// registry or toolchain sources); point the client at our
					// content provider. Paths that exist locally stay file URIs:
					// they may be the client's own unmapped URIs echoed back
					// (e.g. diagnostics for a file opened outside the sync root),
					// which the client can read directly.
					parsedURL.Scheme = graftURIScheme
					parsedURL.Host = args.ConnectionName
					parsedURL.Path = decorateRemotePathSegment(parsedURL.Path, args.ConnectionName)
					v[mapK] = parsedURL.String()
				}
			default:
				rewriteLocalRemoteURIs(mapV, args, forRemote)
			}
		}
	}
}

// injectTextDocumentContentCapability advertises that this proxy serves read-only
// file content for the graft URI scheme, keeping any schemes the server natively
// advertised. The nested form is the LSP 3.18 workspace/textDocumentContent
// server capability; the flat form is the name Sublime Text's LSP package reads.
func injectTextDocumentContentCapability(result any) {
	resultMap, ok := result.(map[string]any)
	if !ok {
		return
	}

	caps, ok := resultMap["capabilities"].(map[string]any)
	if !ok {
		return
	}

	workspace, ok := caps["workspace"].(map[string]any)
	if !ok {
		workspace = map[string]any{}
		caps["workspace"] = workspace
	}

	schemes := []any{}

	if existing, ok := workspace[capTextDocumentContentNested].(map[string]any); ok {
		if existingSchemes, ok := existing[capTextDocumentContentSchemes].([]any); ok {
			schemes = append(schemes, existingSchemes...)
		}
	}

	if !slices.Contains(schemes, any(graftURIScheme)) {
		schemes = append(schemes, graftURIScheme)
	}

	workspace[capTextDocumentContentNested] = map[string]any{capTextDocumentContentSchemes: schemes}
	caps[capTextDocumentContentFlat] = map[string]any{capTextDocumentContentSchemes: schemes}
}

// clientDeclaresTextDocumentContent reports whether initialize request params
// declare the LSP 3.18 workspace.textDocumentContent client capability, meaning
// the client can fetch content for non-file URI schemes.
func clientDeclaresTextDocumentContent(params any) bool {
	paramsMap, ok := params.(map[string]any)
	if !ok {
		return false
	}

	caps, ok := paramsMap["capabilities"].(map[string]any)
	if !ok {
		return false
	}

	workspace, ok := caps["workspace"].(map[string]any)
	if !ok {
		return false
	}

	_, declared := workspace[capTextDocumentContentNested]

	return declared
}

// paramsURIScheme extracts the scheme of a params.uri value, or empty if there
// is none.
func paramsURIScheme(params any) string {
	paramsMap, ok := params.(map[string]any)
	if !ok {
		return ""
	}

	uriStr, ok := paramsMap["uri"].(string)
	if !ok {
		return ""
	}

	parsedURL, err := url.Parse(uriStr)
	if err != nil {
		return ""
	}

	return parsedURL.Scheme
}

// connectionNameIsURIHostSafe reports whether a connection name round trips
// unchanged through the host component of a URI, which minted graft URIs
// require.
func connectionNameIsURIHostSafe(name string) bool {
	if name == "" {
		return false
	}

	minted := (&url.URL{Scheme: graftURIScheme, Host: name, Path: "/probe"}).String()

	parsed, err := url.Parse(minted)
	if err != nil {
		return false
	}

	return strings.EqualFold(parsed.Host, name)
}

// pathBoundaryOK reports whether a prefix match ended on a path boundary,
// preventing /a/bc from matching the prefix /a/b.
func pathBoundaryOK(rest string) bool {
	return rest == "" || strings.HasPrefix(rest, "/")
}

// decorateRemotePathSegment marks the file name of a minted graft URI with the
// connection it came from, keeping the extension last so editors that infer a
// language from it keep working: /a/context.rs becomes /a/context@conn.rs.
// Editors title tabs for virtual documents with the last path segment, so the
// marker is what distinguishes a served remote file from a local one.
func decorateRemotePathSegment(path, connectionName string) string {
	slash := strings.LastIndexByte(path, '/')
	dir, name := path[:slash+1], path[slash+1:]

	if name == "" {
		return path
	}

	marker := "@" + connectionName

	ext := filepath.Ext(name)

	stem := strings.TrimSuffix(name, ext)
	if stem == "" {
		// Dotfiles and the like have no stem to mark; append instead.
		return dir + name + marker
	}

	return dir + stem + marker + ext
}

// stripRemotePathSegment undoes decorateRemotePathSegment, recovering the real
// remote path. Paths without the marker pass through unchanged.
func stripRemotePathSegment(path, connectionName string) string {
	slash := strings.LastIndexByte(path, '/')
	dir, name := path[:slash+1], path[slash+1:]
	marker := "@" + connectionName

	ext := filepath.Ext(name)

	stem := strings.TrimSuffix(name, ext)
	if cut, ok := strings.CutSuffix(stem, marker); ok && cut != "" {
		return dir + cut + ext
	}

	if cut, ok := strings.CutSuffix(name, marker); ok {
		return dir + cut
	}

	return path
}

func localPathExists(path string) bool {
	_, err := os.Stat(path)

	return err == nil
}

// handleTextDocumentContent serves a workspace/textDocumentContent request for a
// graft scheme URI by reading the file from the remote side of the connection.
func handleTextDocumentContent(
	params any,
	args lspRewriterArgs,
	readFile func(path string) (string, error),
) (any, error) {
	paramsMap, ok := params.(map[string]any)
	if !ok {
		return nil, errors.Errorf("expected map params for %s", methodTextDocumentContent)
	}

	uriStr, ok := paramsMap["uri"].(string)
	if !ok {
		return nil, errors.Errorf("expected string uri param for %s", methodTextDocumentContent)
	}

	parsedURL, err := url.Parse(uriStr)
	if err != nil {
		return nil, errors.Wrap(err)
	}

	if parsedURL.Scheme != graftURIScheme {
		return nil, errors.Errorf("unsupported scheme %q for %s", parsedURL.Scheme, methodTextDocumentContent)
	}

	if !strings.EqualFold(parsedURL.Host, args.ConnectionName) {
		return nil, errors.Errorf("URI is for connection %q but session is connected to %q",
			parsedURL.Host, args.ConnectionName)
	}

	text, err := readFile(stripRemotePathSegment(parsedURL.Path, args.ConnectionName))
	if err != nil {
		return nil, errors.Wrap(err)
	}

	return map[string]any{"text": text}, nil
}

// ExecLocalLSP replaces the current process with the given executable, passing
// through any arguments after "graft lsp <executable>".
func ExecLocalLSP(executable string) error {
	execPath, err := exec.LookPath(executable)
	if err != nil {
		return errors.Wrap(err)
	}

	return errors.Wrap(syscall.Exec(execPath, append([]string{executable}, os.Args[3:]...), os.Environ()))
}

type lspRewriterArgs struct {
	Executable     string
	ConnectionName string
	// ClientSupportsContent is set once the client's initialize request declares
	// the workspace.textDocumentContent capability; graft URIs are only minted
	// for clients that can fetch their content.
	ClientSupportsContent *atomic.Bool
	Remappings            []*graftv1.PathRemapping
}

// remoteReadCommand builds the graft run invocation (argv, then stdin payload)
// used to read a remote file. The path travels NUL-terminated via stdin and is
// consumed by `xargs -0`, which passes it to cat as a single literal argument.
// It must not travel via argv: graft run wraps argv in a remote shell string,
// and a further shell layer would expand path characters like $, * and ; (a
// bare "$p" is expanded away by the wrapping shell before the inner shell runs).
func remoteReadCommand(connectionName, path string) ([]string, string, error) {
	if strings.ContainsRune(path, 0) {
		// Impossible in a real filesystem path; guard the delimiter anyway.
		return nil, "", errors.New("remote path contains a NUL byte")
	}

	return []string{
		"run", "--to", connectionName,
		"xargs", "-0", "cat", "--",
	}, path + "\x00", nil
}

// readRemoteFile reads a file from the remote side of the named connection
// through the same plumbing as graft run.
func readRemoteFile(ctx context.Context, connectionName, path string) (string, error) {
	args, stdin, err := remoteReadCommand(connectionName, path)
	if err != nil {
		return "", err
	}

	//nolint:gosec // the executable is ourselves and the argv is fixed; the path travels via stdin
	execCmd := exec.CommandContext(ctx, os.Args[0], args...)

	var stdout, stderr bytes.Buffer

	execCmd.Stdin = strings.NewReader(stdin)
	execCmd.Stdout = &stdout
	execCmd.Stderr = &stderr

	if err := execCmd.Run(); err != nil {
		return "", errors.Errorf("failed to read remote file %q: %w: %s",
			path, err, strings.TrimSpace(stderr.String()))
	}

	return stdout.String(), nil
}

// isGraftDocumentEdit reports whether a client message would modify a
// read-only graft document (content served via workspace/textDocumentContent).
// Conforming clients never edit such documents; dropping these protects the
// real remote file from a misbehaving one.
func isGraftDocumentEdit(method string, params any) bool {
	switch method {
	case "textDocument/didChange", "textDocument/didSave",
		"textDocument/willSave", "textDocument/willSaveWaitUntil":
	default:
		return false
	}

	paramsMap, ok := params.(map[string]any)
	if !ok {
		return false
	}

	return paramsURIScheme(paramsMap["textDocument"]) == graftURIScheme
}
