package exec

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	osexec "os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/fsnotify/fsnotify"

	"github.com/jonasohland/mxl-replicator/internal/worker"
	"github.com/jonasohland/mxl-replicator/internal/worker/logs"
	"github.com/jonasohland/mxl-replicator/internal/worker/metrics"
)

// Start implements [worker.Launcher].
//
// ctx bounds the start and nothing else. The worker outlives it and is stopped only by
// [handle.Stop] — which is why this uses [osexec.Command] and not [osexec.CommandContext],
// the version that looks obviously right and would bind a worker's life to the reconcile pass
// that started it. A cancelled poll would then tear down media, which is fail-static read
// backwards (§4.2).
func (l *Launcher) Start(ctx context.Context, spec worker.Spec) (worker.Handle, error) {
	if err := spec.Validate(); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	// Fresh directory per *start*, not per logical worker. The worker unlinks its socket path
	// before binding now, so reuse is no longer fatal — but a previous incarnation's
	// target-info.json sitting in the directory is, because the inotify wait below would
	// return a blob describing rkeys that died with the process that wrote it (WRS §6).
	dir, err := os.MkdirTemp(l.workRoot, workDirPrefix)
	if err != nil {
		return nil, fmt.Errorf("worker: create work directory: %w", err)
	}
	started := false
	defer func() {
		if !started {
			_ = os.RemoveAll(dir)
		}
	}()

	socket := filepath.Join(dir, metricsSocketName)
	if len(socket) >= sunPathMax {
		return nil, fmt.Errorf("worker: metrics socket path %q is %d bytes, over the %d-byte sun_path limit", socket, len(socket), sunPathMax)
	}

	infoPath := ""
	if spec.IsTarget() {
		infoPath = filepath.Join(dir, targetInfoName)
	}

	configPath := filepath.Join(dir, configName)
	if err := writeConfig(configPath, buildConfig(spec, socket, infoPath)); err != nil {
		return nil, err
	}

	log := l.log.With("session", spec.SessionID, "role", string(spec.Role))
	h := &handle{
		spec:      spec,
		dir:       dir,
		socket:    socket,
		grace:     l.stopGrace,
		log:       log,
		tail:      logs.NewRing(l.tailBytes),
		dead:      make(chan struct{}),
		infoReady: make(chan struct{}),
		exited:    make(chan worker.Exit, 1),
	}

	// Watch before exec. The blob is written early — as soon as the fabric endpoint is bound,
	// before the receive loop starts (WRS §5.1) — so a watch established after the process
	// starts can miss the event and then wait forever for one that already happened.
	if spec.IsTarget() {
		if err := h.watchTargetInfo(dir, infoPath); err != nil {
			return nil, err
		}
	}

	cmd := osexec.Command(l.binary, configPath)
	cmd.Env = l.env
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("worker: stdout pipe: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, fmt.Errorf("worker: stderr pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("worker: start %s: %w", l.binary, err)
	}
	h.cmd = cmd
	started = true
	log.Debug("worker started", "pid", cmd.Process.Pid, "dir", dir)

	// The worker logs everything to stdout, including the fatal line that says why it is
	// about to exit; stderr should be silent in normal operation, which is exactly why
	// anything appearing there is worth surfacing (WRS §7, §8).
	h.pumps.Add(2)
	go h.pump(stdout, "stdout")
	go h.pump(stderr, "stderr")
	go h.wait()

	return h, nil
}

// handle is one running worker process.
type handle struct {
	spec   worker.Spec
	cmd    *osexec.Cmd
	dir    string
	socket string
	grace  time.Duration
	log    *slog.Logger

	pumps sync.WaitGroup

	// tail is the retained end of this worker's output (§12.2). Per *start*, like the work
	// directory: a restart is a new incarnation and its predecessor's output explains a different
	// failure.
	tail *logs.Ring

	// dead closes once the process has exited and its output has been drained.
	dead   chan struct{}
	exited chan worker.Exit

	// infoReady closes once info holds a complete blob. Target role only.
	infoReady chan struct{}
	infoOnce  sync.Once

	mu       sync.Mutex
	info     string
	stopping bool

	stopMu    sync.Mutex
	cleanedUp bool
}

var _ worker.Handle = (*handle)(nil)

// Spec implements [worker.Handle].
func (h *handle) Spec() worker.Spec { return h.spec }

// LogTail implements [worker.Handle].
func (h *handle) LogTail() string { return h.tail.Text() }

// Exited implements [worker.Handle].
func (h *handle) Exited() <-chan worker.Exit { return h.exited }

// TargetInfo implements [worker.Handle].
func (h *handle) TargetInfo(ctx context.Context) (string, error) {
	if !h.spec.IsTarget() {
		return "", worker.ErrNotTarget
	}

	// Checked before the select rather than left as one of its cases, because select picks
	// randomly among ready channels and the answer here is not a coin flip: a dead target's
	// blob describes memory registrations that died with it, so it is worse than no answer.
	select {
	case <-h.dead:
		return "", worker.ErrExited
	default:
	}

	select {
	case <-h.infoReady:
		h.mu.Lock()
		defer h.mu.Unlock()
		return h.info, nil
	case <-h.dead:
		return "", worker.ErrExited
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

// Metrics implements [worker.Handle].
func (h *handle) Metrics(ctx context.Context) ([]worker.Sample, error) {
	select {
	case <-h.dead:
		return nil, worker.ErrExited
	default:
	}
	return metrics.Scrape(ctx, h.socket)
}

// Stop implements [worker.Handle]: SIGTERM, a grace period, SIGKILL, then the work directory.
//
// Idempotent and safe on a worker that has already died — which is the normal way it is
// called, since the agent stops a dead handle before starting its replacement. The directory
// survives until then on purpose: a worker that is failing to start leaves its config.json and
// its logs behind for as long as the agent keeps it, and only stopping it clears them.
func (h *handle) Stop(ctx context.Context) error {
	h.stopMu.Lock()
	defer h.stopMu.Unlock()

	if h.cleanedUp {
		return nil
	}

	h.mu.Lock()
	h.stopping = true
	h.mu.Unlock()

	h.terminate(ctx)
	<-h.dead

	h.cleanedUp = true
	if err := os.RemoveAll(h.dir); err != nil {
		return fmt.Errorf("worker: remove work directory: %w", err)
	}
	return nil
}

// terminate walks the signal sequence. It returns once the process is gone, whatever ctx says:
// a worker left running holds a port, a memory registration and a flow, so giving up on it is
// not an option the caller gets to choose (§7.4).
func (h *handle) terminate(ctx context.Context) {
	select {
	case <-h.dead:
		return
	default:
	}

	if err := h.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		h.log.Debug("signalling worker failed, it is probably already gone", "error", err)
	}

	grace := time.NewTimer(h.grace)
	defer grace.Stop()

	select {
	case <-h.dead:
		return
	case <-ctx.Done():
		// The caller is out of patience. Escalate rather than return with the process still
		// running: an unstopped worker is worse for the caller than a slow Stop.
		h.log.Warn("stop deadline reached, killing worker", "grace", h.grace)
	case <-grace.C:
		h.log.Warn("worker did not exit on SIGTERM, killing", "grace", h.grace)
	}

	if err := h.cmd.Process.Kill(); err != nil {
		h.log.Debug("killing worker failed, it is probably already gone", "error", err)
	}
}

// wait reaps the process and publishes the exit.
func (h *handle) wait() {
	// The pipes must be drained before Wait, which closes them underneath any reader still
	// working — the documented contract of StdoutPipe.
	h.pumps.Wait()

	err := h.cmd.Wait()

	h.mu.Lock()
	stopped := h.stopping
	h.mu.Unlock()

	h.log.Debug("worker exited", "stopped", stopped, "error", err)

	close(h.dead)
	h.exited <- worker.Exit{At: time.Now(), Stopped: stopped, Err: err}
	close(h.exited)
}

// pump re-emits one of the worker's output streams through the agent's logger.
func (h *handle) pump(r io.Reader, stream string) {
	defer h.pumps.Done()

	log := h.log.With("module", "worker")
	scanner := bufio.NewScanner(r)
	// Worker log lines are short, but a flow definition in an error message is not, and a
	// truncated line is a truncated diagnostic.
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}
		// Into the tail *before* parsing, and whether or not it parses (§12.2). A tail holding
		// only what this parser understood would omit whatever a library on the link line printed
		// on its way out, which is the case where the parser is least likely to be right and the
		// output most likely to matter.
		h.tail.Add(line)

		record, ok := logs.Parse(line)
		if !ok {
			// Not a spdlog line: libfabric's own diagnostics go to the same stream, and so
			// does anything else the process decides to print. Emit it rather than dropping
			// it — the legacy translator dropped these, which is a good way to lose the one
			// message explaining a failure.
			log.Warn(line, "stream", stream)
			continue
		}
		if !log.Handler().Enabled(context.Background(), record.Level) {
			continue
		}
		_ = log.Handler().Handle(context.Background(), record)
	}
}

// watchTargetInfo waits for the target's blob and captures it.
//
// inotify rather than the legacy 200 ms → 2 s backoff poll, because this sits directly on the
// establishment path: the peer's initiator cannot be assigned until the epoch computed from
// this blob has been reported (§5.3), and §6.1 wants the whole re-establishment inside 1–2 s.
func (h *handle) watchTargetInfo(dir, path string) error {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return fmt.Errorf("worker: watch work directory: %w", err)
	}
	if err := watcher.Add(dir); err != nil {
		_ = watcher.Close()
		return fmt.Errorf("worker: watch %s: %w", dir, err)
	}

	go func() {
		defer func() { _ = watcher.Close() }()
		for {
			// Read first, then wait. The file may already be there — this runs before exec on
			// the first pass, but a missed or coalesced event later must not be able to strand
			// the wait either.
			if blob, ok := readTargetInfo(path); ok {
				h.mu.Lock()
				h.info = blob
				h.mu.Unlock()
				h.infoOnce.Do(func() { close(h.infoReady) })
				return
			}

			select {
			case _, open := <-watcher.Events:
				if !open {
					return
				}
			case err, open := <-watcher.Errors:
				if !open {
					return
				}
				h.log.Warn("watching for target info failed", "error", err)
			case <-h.dead:
				return
			}
		}
	}()

	return nil
}

// readTargetInfo reads the blob, if it is there and complete.
//
// The worker writes the file with an ordinary buffered stream rather than an atomic rename, so
// a Create event can arrive with nothing behind it yet. Requiring valid JSON is what makes a
// partial read wait for the rest instead of handing the agent half a blob, which would fail to
// decode later and look like a corrupt target rather than a race.
func readTargetInfo(path string) (string, bool) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", false
	}

	// Older workers wrote a trailing NUL, because the library's reported size counts the
	// terminator. Fixed at the source, but a mixed-version deployment must not look like a
	// corrupt blob — and json.Valid says no to a NUL after the top-level value.
	blob := bytes.TrimRight(raw, "\x00 \t\r\n")
	if len(blob) == 0 || !json.Valid(blob) {
		return "", false
	}
	return string(blob), true
}
