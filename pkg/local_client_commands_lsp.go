package graft

import (
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
	lspArgs.Remappings = selectResp.GetPathRemappings()

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

	// TODO(erd): trim over time
	// TODO(erd): cant we just use a jsonrpc2.Connection instead as the abstraction to the command? probably
	var inflightMu sync.Mutex

	inflightClientRequests := map[any]chan any{}

	activeWorkers.Go(func() {
		frameReader := headerFramer.Reader(stdoutPipe)
		frameWriter := headerFramer.Writer(stdinPipe)

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
					_, _ = frameWriter.Write(bindCtx, &jsonrpc2.Response{ID: v.ID, Error: err}) //nolint:errcheck

					continue
				}

				rewriteLocalRemoteURIs(params, l.args.Remappings, false)
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
					defer func() {
					}()
					defer serverRequestsWaiting.Add(-1)

					var result any
					if err := clientResp.Await(bindCtx, &result); err != nil {
						_, _ = frameWriter.Write(bindCtx, &jsonrpc2.Response{ID: v.ID, Error: err}) //nolint:errcheck

						return
					}

					rewriteLocalRemoteURIs(result, l.args.Remappings, true)

					md, err := json.Marshal(result)
					if err != nil {
						return
					}

					if _, err := frameWriter.Write(bindCtx, &jsonrpc2.Response{ID: v.ID, Result: md}); err != nil {
						return
					}
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

					rewriteLocalRemoteURIs(results, l.args.Remappings, false)
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
				return nil, errors.Wrap(err)
			}

			rewriteLocalRemoteURIs(params, l.args.Remappings, true)

			md, err := json.Marshal(params)
			if err != nil {
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

			// TODO(erd): do we need to actually get the right responses to messages?
			if _, err := headerFramer.Writer(stdinPipe).Write(bindCtx, req); err != nil {
				return nil, errors.Wrap(err)
			}

			if respCh == nil {
				clientRequestsWaiting.Add(-1)

				//nolint:nilnil // notification means no response or error
				return nil, nil
			}

			defer clientRequestsWaiting.Add(-1)

			select {
			case <-bindCtx.Done():
				return nil, bindCtx.Err()
			case resp := <-respCh:
				if err, ok := resp.(error); ok {
					return nil, errors.Wrap(err)
				} else {
					return resp, nil
				}
			}
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
func rewriteLocalRemoteURIs(value any, remappings []*graftv1.PathRemapping, forRemote bool) {
	switch v := value.(type) {
	case []any:
		for _, elem := range v {
			rewriteLocalRemoteURIs(elem, remappings, forRemote)
		}
	case map[string]any:
		for mapK, mapV := range v {
			switch mapK {
			case "globPattern":
				vStr, ok := mapV.(string)
				if !ok {
					continue
				}

				for _, mapping := range remappings {
					var from, to string
					if forRemote {
						from = mapping.GetFromPrefix()
						to = mapping.GetToPrefix()
					} else {
						from = mapping.GetToPrefix()
						to = mapping.GetFromPrefix()
					}

					cutStr, found := strings.CutPrefix(vStr, from)
					if !found {
						continue
					}

					v[mapK] = filepath.Join(to, cutStr)

					break
				}
			case "uri", "rootUri", "scopeUri":
				vStr, ok := mapV.(string)
				if !ok {
					continue
				}

				parsedURL, err := url.Parse(vStr)
				if err != nil {
					continue
				}

				if parsedURL.Scheme != "file" {
					continue
				}

				for _, mapping := range remappings {
					var from, to string
					if forRemote {
						from = mapping.GetFromPrefix()
						to = mapping.GetToPrefix()
					} else {
						from = mapping.GetToPrefix()
						to = mapping.GetFromPrefix()
					}

					cutStr, found := strings.CutPrefix(parsedURL.Path, from)
					if !found {
						continue
					}

					parsedURL.Path = filepath.Join(to, cutStr)
					v[mapK] = parsedURL.String()

					break
				}
			default:
				rewriteLocalRemoteURIs(mapV, remappings, forRemote)
			}
		}
	}
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
	Executable string
	Remappings []*graftv1.PathRemapping
}
