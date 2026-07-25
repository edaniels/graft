package graft

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/edaniels/graft/errors"
	graftv1 "github.com/edaniels/graft/gen/proto/graft/v1"
)

const (
	// defaultOutputRingCapacity bounds how much recent output is retained per
	// stream for detached commands and attach replay.
	defaultOutputRingCapacity = 1 << 20

	// defaultKillGrace is how long a terminated command group gets to exit
	// before escalation to SIGKILL.
	defaultKillGrace = 5 * time.Second

	// defaultDetachKillDelay is how long a kill-policy command survives after
	// its client stream breaks without an explicit goodbye. Deliberate exits
	// (Ctrl-C, LSP shutdown) are killed immediately via an explicit
	// KillCommand from the local daemon, which can tell a client exit from a
	// transport break; this window therefore only has to cover transport
	// loss, and it is deliberately generous so laptop sleeps and long network
	// outages resume instead of killing work (the tailscale/mosh
	// expectation). Its cost is only paid when the client machine never
	// comes back.
	defaultDetachKillDelay = time.Hour

	// defaultDrainGrace is how long after the command process exits its
	// output drain may keep running before the daemon forcibly closes the
	// command's pty/pipes. Descendants that inherited the output fds (e.g. a
	// background job left in a shell) would otherwise keep the drain open
	// forever, leaving the command permanently "running" and wedging daemon
	// shutdown.
	defaultDrainGrace = 2 * time.Second
)

// commandPersistence is the resolved lifetime behavior of a managed command
// when its client stream goes away before it exits.
type commandPersistence int

const (
	// persistenceKill terminates the command's process group on disconnect.
	persistenceKill commandPersistence = iota
	// persistenceKeep leaves the command running detached for later re-attach.
	persistenceKeep
)

func (p commandPersistence) proto() graftv1.CommandPersistence {
	if p == persistenceKeep {
		return graftv1.CommandPersistence_COMMAND_PERSISTENCE_KEEP
	}

	return graftv1.CommandPersistence_COMMAND_PERSISTENCE_KILL
}

// resolvePersistence maps a requested persistence to a concrete one: unless
// explicitly chosen, interactive (pty) commands and shells persist like a
// terminal multiplexer session while plain piped commands die with their
// client.
func resolvePersistence(requested graftv1.CommandPersistence, pty, shell bool) commandPersistence {
	switch requested {
	case graftv1.CommandPersistence_COMMAND_PERSISTENCE_KEEP:
		return persistenceKeep
	case graftv1.CommandPersistence_COMMAND_PERSISTENCE_KILL:
		return persistenceKill
	case graftv1.CommandPersistence_COMMAND_PERSISTENCE_UNKNOWN:
		fallthrough
	default:
		if pty || shell {
			return persistenceKeep
		}

		return persistenceKill
	}
}

// commandStateEntry is one running command in the on-disk state file, used at
// startup to clean up process groups a prior daemon incarnation left behind.
type commandStateEntry struct {
	ID            string `json:"id"`
	PID           int    `json:"pid"`
	StartedAtUnix int64  `json:"startedAtUnix"`
	Display       string `json:"display"`
}

// ManagedCommandSpec describes a command being registered.
type ManagedCommandSpec struct {
	Display     string
	CWD         string
	Pty         bool
	Persistence commandPersistence

	// StdoutCapacity/StderrCapacity override the ring buffer sizes (defaulted
	// when zero); primarily for tests.
	StdoutCapacity int
	StderrCapacity int
}

// commandAttachment identifies one client attached to a managed command. A
// newer attachment steals the command; the old holder observes Canceled.
type commandAttachment struct {
	canceled chan struct{}
}

// Canceled is closed when a newer attachment has taken over the command.
func (a *commandAttachment) Canceled() <-chan struct{} {
	return a.canceled
}

// A ManagedCommand is a locally running command owned by the daemon rather
// than by any single client stream. Its output is continuously drained into
// bounded ring buffers (so it can never wedge on a full pty/pipe with nobody
// reading), and clients attach to and detach from it over time.
type ManagedCommand struct {
	id          string
	display     string
	cwd         string
	pty         bool
	persistence commandPersistence
	startedAt   time.Time

	cmd        *LocalRunningCommand
	stdoutRing *outputRing
	stderrRing *outputRing

	reg     *CommandRegistry
	drainWg sync.WaitGroup
	done    chan struct{}

	mu          sync.Mutex
	current     *commandAttachment
	pendingKill *time.Timer // delayed kill-policy kill; canceled by a re-attach
	exited      bool
	exitedAt    time.Time
	exitStatus  int
	exitErr     error
}

// ID returns the command's registry identifier.
func (mc *ManagedCommand) ID() string { return mc.id }

// PID returns the command's process ID (also its process group ID).
func (mc *ManagedCommand) PID() int { return mc.cmd.PID() }

// Pty returns whether the command runs under a pseudoterminal.
func (mc *ManagedCommand) Pty() bool { return mc.pty }

// Stdin returns the command's stdin writer.
func (mc *ManagedCommand) Stdin() io.WriteCloser { return mc.cmd.Stdin() }

// Persistence returns the command's current lifetime policy (mutable: an
// explicit detach flips a command to keep).
func (mc *ManagedCommand) currentPersistence() commandPersistence {
	mc.mu.Lock()
	defer mc.mu.Unlock()

	return mc.persistence
}

// CloseStdinFromClient handles a client's stdin half-close. For kill-policy
// commands the EOF propagates (piped one-shots rely on it to finish); keep
// commands hold stdin open so a later attachment can still provide input.
func (mc *ManagedCommand) CloseStdinFromClient() error {
	if mc.currentPersistence() != persistenceKill {
		return nil
	}

	return errors.Wrap(mc.cmd.Stdin().Close())
}

// DetachAndKeep disconnects the current attachment (if any) and flips the
// command to keep persistence: an explicit detach means "let it run". Any
// pending disconnect-kill is called off. Reports whether a client was
// actually attached.
func (mc *ManagedCommand) DetachAndKeep() bool {
	mc.mu.Lock()
	defer mc.mu.Unlock()

	if mc.pendingKill != nil {
		mc.pendingKill.Stop()
		mc.pendingKill = nil
	}

	mc.persistence = persistenceKeep

	if mc.current == nil {
		return false
	}

	close(mc.current.canceled)
	mc.current = nil

	return true
}

// currentIs reports whether the handle is still the command's current
// attachment.
func (mc *ManagedCommand) currentIs(handle *commandAttachment) bool {
	mc.mu.Lock()
	defer mc.mu.Unlock()

	return mc.current == handle
}

// AckOutput records how much output the client has confirmed consuming,
// releasing that space for lossless (kill-policy) writers.
func (mc *ManagedCommand) AckOutput(stdoutOffset, stderrOffset uint64) {
	mc.stdoutRing.setReleased(stdoutOffset)
	mc.stderrRing.setReleased(stderrOffset)
}

// outRing returns the ring holding drained stdout.
func (mc *ManagedCommand) outRing() *outputRing { return mc.stdoutRing }

// errRing returns the ring holding drained stderr.
func (mc *ManagedCommand) errRing() *outputRing { return mc.stderrRing }

// Signal signals the command's process group.
func (mc *ManagedCommand) Signal(sig string) error { return mc.cmd.Signal(sig) }

// SetEnvVar delegates to the underlying command.
func (mc *ManagedCommand) SetEnvVar(key, value string) error { return mc.cmd.SetEnvVar(key, value) }

// NotifyWindowChange delegates to the underlying command's pty.
func (mc *ManagedCommand) NotifyWindowChange(h, w int) error { return mc.cmd.NotifyWindowChange(h, w) }

// Done is closed once the command has exited and its output is fully drained.
func (mc *ManagedCommand) Done() <-chan struct{} { return mc.done }

// ExitStatus returns the command's exit status (and whether it has exited at
// all) once Done is closed.
func (mc *ManagedCommand) ExitStatus() (int, bool, error) {
	mc.mu.Lock()
	defer mc.mu.Unlock()

	return mc.exitStatus, mc.exited, mc.exitErr
}

// attach registers a client with the command, stealing it from any current
// attachment (the previous holder's handle is canceled). A pending
// disconnect-kill is called off: the client came back.
func (mc *ManagedCommand) attach() *commandAttachment {
	mc.mu.Lock()
	defer mc.mu.Unlock()

	if mc.pendingKill != nil {
		mc.pendingKill.Stop()
		mc.pendingKill = nil
	}

	if mc.current != nil {
		close(mc.current.canceled)
	}

	mc.current = &commandAttachment{canceled: make(chan struct{})}

	return mc.current
}

// ClientGone reports that the holder of the given attachment disconnected. If
// it is still the current attachment, the command's persistence policy is
// applied: keep commands stay running detached; kill commands get a short
// re-attach window (a transient transport break is indistinguishable from a
// client that exited) before their process group is terminated.
func (mc *ManagedCommand) ClientGone(handle *commandAttachment) {
	mc.mu.Lock()
	defer mc.mu.Unlock()

	if mc.current != handle {
		return
	}

	mc.current = nil

	if mc.exited || mc.persistence != persistenceKill {
		return
	}

	if mc.pendingKill != nil {
		mc.pendingKill.Stop()
	}

	mc.pendingKill = time.AfterFunc(mc.reg.detachKillDelay, func() {
		go mc.killGroupWithGrace(mc.reg.killGrace)
	})
}

// KillWithEscalation gracefully terminates the command's process group,
// escalating to SIGKILL after the registry's grace period.
func (mc *ManagedCommand) KillWithEscalation() {
	go mc.killGroupWithGrace(mc.reg.killGrace)
}

// killGroupWithGrace terminates the command's process group, escalating to
// SIGKILL if it has not exited after the grace period.
func (mc *ManagedCommand) killGroupWithGrace(grace time.Duration) {
	if _, exited, _ := mc.ExitStatus(); exited { //nolint:errcheck // liveness peek only
		// Already gone; signaling the recorded (possibly reused) pid would be
		// worse than doing nothing.
		return
	}

	if err := mc.Signal(SignalTerminate); err != nil {
		slog.Debug("error terminating command group", "id", mc.id, "error", err)
	}

	select {
	case <-mc.done:
		return
	case <-time.After(grace):
	}

	if err := mc.Signal(SignalKill); err != nil {
		slog.Debug("error killing command group", "id", mc.id, "error", err)
	}
}

// info renders the command for listing.
func (mc *ManagedCommand) info() *graftv1.CommandInfo {
	mc.mu.Lock()
	defer mc.mu.Unlock()

	return &graftv1.CommandInfo{
		CommandId:     mc.id,
		Command:       mc.display,
		Cwd:           mc.cwd,
		Pty:           mc.pty,
		Running:       !mc.exited,
		ExitStatus:    int64(mc.exitStatus),
		StartedAtUnix: mc.startedAt.Unix(),
		Attached:      mc.current != nil,
		Persistence:   mc.persistence.proto(),
	}
}

// A CommandRegistry owns all managed commands of a daemon. It keeps an
// on-disk state file of running process groups so a fresh daemon can clean up
// what a crashed predecessor left behind.
type CommandRegistry struct {
	statePath       string
	killGrace       time.Duration
	detachKillDelay time.Duration
	drainGrace      time.Duration

	mu       sync.Mutex
	commands map[string]*ManagedCommand
	closed   bool
}

// NewCommandRegistry returns a registry persisting running-command state to
// statePath (empty disables persistence).
func NewCommandRegistry(statePath string) *CommandRegistry {
	return &CommandRegistry{
		statePath:       statePath,
		killGrace:       defaultKillGrace,
		detachKillDelay: defaultDetachKillDelay,
		drainGrace:      defaultDrainGrace,
		commands:        map[string]*ManagedCommand{},
	}
}

// Register takes ownership of a started command: its output begins draining
// into ring buffers immediately and its exit is reaped by the registry.
func (reg *CommandRegistry) Register(cmd *LocalRunningCommand, spec ManagedCommandSpec) (*ManagedCommand, error) {
	stdoutCapacity := spec.StdoutCapacity
	if stdoutCapacity == 0 {
		stdoutCapacity = defaultOutputRingCapacity
	}

	stderrCapacity := spec.StderrCapacity
	if stderrCapacity == 0 {
		stderrCapacity = defaultOutputRingCapacity
	}

	mc := &ManagedCommand{
		display:     spec.Display,
		cwd:         spec.CWD,
		pty:         spec.Pty,
		persistence: spec.Persistence,
		startedAt:   time.Now(),
		cmd:         cmd,
		stdoutRing:  newOutputRing(stdoutCapacity),
		stderrRing:  newOutputRing(stderrCapacity),
		reg:         reg,
		done:        make(chan struct{}),
	}

	// Kill-policy commands are the piped/programmatic ones (scripts, LSP):
	// their output is data, so they get TCP-like backpressure - writes block
	// once unconfirmed output fills the buffer - instead of the terminal-style
	// evict-oldest that keep commands use.
	if spec.Persistence == persistenceKill {
		mc.stdoutRing.setLossless()
		mc.stderrRing.setLossless()
	}

	reg.mu.Lock()

	if reg.closed {
		reg.mu.Unlock()

		return nil, errors.New("command registry is closed")
	}

	id, err := reg.newUniqueCommandIDLocked()
	if err != nil {
		reg.mu.Unlock()

		return nil, err
	}

	mc.id = id
	reg.commands[id] = mc
	reg.writeStateLocked()
	reg.mu.Unlock()

	mc.drainWg.Add(1)

	go drainIntoRing(cmd.Stdout(), mc.stdoutRing, &mc.drainWg)

	if cmd.Stdout() != cmd.Stderr() {
		mc.drainWg.Add(1)

		go drainIntoRing(cmd.Stderr(), mc.stderrRing, &mc.drainWg)
	} else {
		// A plain interactive pty has a single combined stream; don't read the
		// pty twice.
		mc.stderrRing.Close()
	}

	go reg.waitAndReap(mc)

	return mc, nil
}

// drainIntoRing continuously consumes a command output stream into its ring.
// This runs regardless of whether any client is attached; it is what prevents
// a detached command from blocking on a full pty/pipe buffer.
func drainIntoRing(reader io.Reader, ring *outputRing, wg *sync.WaitGroup) {
	defer wg.Done()
	defer ring.Close()

	var buf [4096]byte

	for {
		n, err := reader.Read(buf[:])
		if n > 0 {
			if _, writeErr := ring.Write(buf[:n]); writeErr != nil {
				return
			}
		}

		if err != nil {
			return
		}
	}
}

// waitAndReap waits for the command to exit, finishes draining, records the
// exit, and updates the state file. The command stays listed (as exited)
// until removed by exit-status delivery or the reaper.
func (reg *CommandRegistry) waitAndReap(mc *ManagedCommand) {
	status, waitErr := mc.cmd.Wait()

	// The drain normally ends moments after the process exits (write ends
	// close, reads hit EOF). A descendant that inherited the output fds (e.g.
	// a background job left behind in a shell) would hold the drain open
	// forever though; after a grace period, force the command's pty/pipes
	// closed so the exit gets recorded and shutdown can't wedge on it.
	drained := make(chan struct{})

	go func() {
		mc.drainWg.Wait()
		close(drained)
	}()

	select {
	case <-drained:
	case <-time.After(reg.drainGrace):
		slog.Debug("output drain outlived the command; forcing it closed", "id", mc.id)
		mc.cmd.ForceCloseOutputs()
		// A lossless drainer can also be blocked writing into a full ring
		// with no client left to confirm; closing the rings unblocks it.
		mc.stdoutRing.Close()
		mc.stderrRing.Close()
		<-drained
	}

	mc.cmd.Release()
	mc.stdoutRing.Close()
	mc.stderrRing.Close()

	mc.mu.Lock()

	if mc.pendingKill != nil {
		mc.pendingKill.Stop()
		mc.pendingKill = nil
	}

	mc.exited = true
	mc.exitedAt = time.Now()
	mc.exitStatus = status
	mc.exitErr = waitErr
	// A kill-policy command that exits with nobody attached has no one left
	// to deliver its status to by design; forget it immediately.
	forget := mc.persistence == persistenceKill && mc.current == nil
	mc.mu.Unlock()

	close(mc.done)

	reg.mu.Lock()

	if forget {
		delete(reg.commands, mc.id)
	}

	reg.writeStateLocked()
	reg.mu.Unlock()
}

// Get returns the managed command with the given id.
func (reg *CommandRegistry) Get(id string) (*ManagedCommand, bool) {
	reg.mu.Lock()
	defer reg.mu.Unlock()

	mc, ok := reg.commands[id]

	return mc, ok
}

// List renders all managed commands, oldest first.
func (reg *CommandRegistry) List() []*graftv1.CommandInfo {
	reg.mu.Lock()

	commands := make([]*ManagedCommand, 0, len(reg.commands))
	for _, mc := range reg.commands {
		commands = append(commands, mc)
	}

	reg.mu.Unlock()

	slices.SortFunc(commands, func(a, b *ManagedCommand) int {
		return a.startedAt.Compare(b.startedAt)
	})

	infos := make([]*graftv1.CommandInfo, 0, len(commands))
	for _, mc := range commands {
		infos = append(infos, mc.info())
	}

	return infos
}

// Remove forgets a command (typically after its exit status was delivered).
func (reg *CommandRegistry) Remove(id string) {
	reg.mu.Lock()
	defer reg.mu.Unlock()

	delete(reg.commands, id)
	reg.writeStateLocked()
}

// ReapExited removes exited, unattached commands older than ttl. It keeps
// short-lived detached commands (e.g. an interactive `ls` whose terminal
// vanished mid-run) from accumulating forever.
func (reg *CommandRegistry) ReapExited(ttl time.Duration) {
	reg.mu.Lock()
	defer reg.mu.Unlock()

	var removed bool

	for id, mc := range reg.commands {
		mc.mu.Lock()
		reapable := mc.exited && mc.current == nil && time.Since(mc.exitedAt) >= ttl
		mc.mu.Unlock()

		if reapable {
			delete(reg.commands, id)

			removed = true
		}
	}

	if removed {
		reg.writeStateLocked()
	}
}

// CloseAll terminates every managed command's process group (with SIGKILL
// escalation) and blocks until they have exited. Used at daemon shutdown:
// without a daemon there is nobody to drain or re-attach, so leaving
// commands running would only orphan them.
func (reg *CommandRegistry) CloseAll() {
	reg.mu.Lock()
	reg.closed = true
	commands := make([]*ManagedCommand, 0, len(reg.commands))

	for _, mc := range reg.commands {
		commands = append(commands, mc)
	}

	reg.mu.Unlock()

	var wg sync.WaitGroup

	for _, mc := range commands {
		select {
		case <-mc.done:
			continue
		default:
		}

		wg.Add(1)

		go func(mc *ManagedCommand) {
			defer wg.Done()

			mc.killGroupWithGrace(reg.killGrace)

			// Bounded: the drain-grace force-close makes done reliably close
			// once the process is dead, but an unkillable process (e.g. stuck
			// in uninterruptible sleep) must not wedge daemon shutdown.
			select {
			case <-mc.done:
			case <-time.After(reg.killGrace + reg.drainGrace + 3*time.Second):
				slog.Warn("command did not finish during shutdown; abandoning it",
					"id", mc.id, "pid", mc.PID(), "command", mc.display)
			}
		}(mc)
	}

	wg.Wait()

	reg.mu.Lock()
	reg.writeStateLocked()
	reg.mu.Unlock()
}

// ReconcileStale kills process groups recorded by a prior daemon incarnation.
// Without a daemon holding their ptys and rings, such processes are
// unreachable and eventually wedge; a fresh daemon cleans them up. Guards:
// the pid must still be a process group leader (our commands always are) and,
// on Linux, must carry the _GRAFT_SPAWNED environment marker.
func (reg *CommandRegistry) ReconcileStale() {
	if reg.statePath == "" {
		return
	}

	stateRd, err := os.ReadFile(reg.statePath)
	if err != nil {
		return
	}

	var entries []commandStateEntry
	if err := json.Unmarshal(stateRd, &entries); err != nil {
		slog.Warn("unreadable command state file; resetting", "path", reg.statePath, "error", err)
	}

	for _, entry := range entries {
		if entry.PID <= 0 {
			continue
		}

		// The group id equals the leader pid we spawned. If the leader is
		// still alive it must both lead its group and carry the graft spawn
		// marker (PID reuse guard). If the leader is gone but the group still
		// has members, those members can only be descendants of our group
		// (group ids are not reused while members remain).
		if pgid, pgErr := syscall.Getpgid(entry.PID); pgErr == nil {
			if pgid != entry.PID || !processLooksGraftSpawned(entry.PID, entry.StartedAtUnix) {
				continue
			}
		} else if syscall.Kill(-entry.PID, syscall.Signal(0)) != nil {
			continue
		}

		slog.Info("terminating command group left behind by a previous daemon",
			"id", entry.ID, "pid", entry.PID, "command", entry.Display)

		if killErr := syscall.Kill(-entry.PID, syscall.SIGTERM); killErr != nil {
			slog.Debug("error terminating stale command group", "pid", entry.PID, "error", killErr)

			continue
		}

		go escalateStaleKill(entry.PID, entry.StartedAtUnix, reg.killGrace)
	}

	reg.mu.Lock()
	reg.writeStateLocked()
	reg.mu.Unlock()
}

// escalateStaleKill force-kills a stale process group if it survived SIGTERM
// past the grace period. The group (not just the leader) is probed for
// liveness: a group id cannot be reused while any member remains, so members
// that outlived their leader are still covered.
func escalateStaleKill(pid int, startedAtUnix int64, grace time.Duration) {
	timer := time.NewTimer(grace)
	defer timer.Stop()

	<-timer.C

	if err := syscall.Kill(-pid, syscall.Signal(0)); err != nil {
		return
	}

	// If the leader itself is alive it must still look like ours: the group
	// was probed above, but the leader pid alone could have been recycled
	// into a fresh group leader in the interim.
	if pgid, pgErr := syscall.Getpgid(pid); pgErr == nil {
		if pgid != pid || !processLooksGraftSpawned(pid, startedAtUnix) {
			return
		}
	}

	if err := syscall.Kill(-pid, syscall.SIGKILL); err != nil {
		slog.Debug("error killing stale command group", "pid", pid, "error", err)
	}
}

// processLooksGraftSpawned reports whether the pid appears to be the command
// graft recorded: on Linux via the _GRAFT_SPAWNED environment marker set on
// every spawned command, on macOS by matching the process start time against
// the recorded one (the environment is not inspectable there). On other
// platforms it reports false so reconcile never signals a process it cannot
// vouch for.
func processLooksGraftSpawned(pid int, startedAtUnix int64) bool {
	switch runtime.GOOS {
	case osLinux:
		environRd, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/environ")
		if err != nil {
			return false
		}

		return strings.Contains(string(environRd), "_GRAFT_SPAWNED=true")
	case osDarwin:
		if startedAtUnix == 0 {
			return false
		}

		//nolint:noctx,gosec // quick local probe; the only variable input is an integer pid
		out, err := exec.Command("ps", "-o", "lstart=", "-p", strconv.Itoa(pid)).Output()
		if err != nil {
			return false
		}

		started, parseErr := time.ParseInLocation(
			"Mon Jan  2 15:04:05 2006", strings.TrimSpace(string(out)), time.Local) //nolint:gosmopolitan // ps prints machine-local time
		if parseErr != nil {
			return false
		}

		// A recycled pid virtually never starts within the same second as the
		// process it replaced.
		diff := started.Unix() - startedAtUnix
		if diff < 0 {
			diff = -diff
		}

		return diff <= 2
	default:
		return false
	}
}

// newUniqueCommandIDLocked generates a command id not already in use.
// Assumes reg.mu held.
func (reg *CommandRegistry) newUniqueCommandIDLocked() (string, error) {
	for range 8 {
		id, err := newCommandID()
		if err != nil {
			return "", err
		}

		if _, exists := reg.commands[id]; !exists {
			return id, nil
		}
	}

	return "", errors.New("could not generate a unique command id")
}

// writeStateLocked persists the running command set. Assumes reg.mu held.
func (reg *CommandRegistry) writeStateLocked() {
	if reg.statePath == "" {
		return
	}

	entries := make([]commandStateEntry, 0, len(reg.commands))

	for _, mc := range reg.commands {
		mc.mu.Lock()
		exited := mc.exited
		mc.mu.Unlock()

		if exited {
			continue
		}

		entries = append(entries, commandStateEntry{
			ID:            mc.id,
			PID:           mc.PID(),
			StartedAtUnix: mc.startedAt.Unix(),
			Display:       mc.display,
		})
	}

	entriesJSON, err := json.Marshal(entries)
	if err != nil {
		slog.Error("error marshaling command state", "error", err)

		return
	}

	// Write-then-rename so a crash mid-write can't leave a truncated file
	// that would make the next daemon skip cleaning up stale process groups.
	tmpPath := reg.statePath + ".tmp"
	if err := os.WriteFile(tmpPath, entriesJSON, 0o600); err != nil {
		slog.Error("error writing command state file", "path", tmpPath, "error", err)

		return
	}

	if err := os.Rename(tmpPath, reg.statePath); err != nil {
		slog.Error("error replacing command state file", "path", reg.statePath, "error", err)
	}
}

// newCommandID returns a short random command identifier.
func newCommandID() (string, error) {
	var raw [4]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", errors.WrapPrefix(err, "error generating command id")
	}

	return hex.EncodeToString(raw[:]), nil
}
