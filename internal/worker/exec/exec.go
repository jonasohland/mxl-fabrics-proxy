// Package exec runs the real mxl-replicator-worker.
//
// It is the only place in the project that starts a process (invariant 11): everything above
// it talks to [worker.Launcher], which is what keeps the control plane testable without MXL,
// libfabric or RDMA hardware, and what would make a future multi-flow worker a substitution
// rather than a rewrite (§14, §17).
//
// What this package owns, all of it a property of *this* worker rather than of the concept:
// a work directory per start, the JSON config file, the process, the inotify wait for a
// target's blob, the AF_UNIX metrics socket, log translation, and the SIGTERM/SIGKILL
// sequence. It owns none of the policy — restart backoff, port allocation, domain-name
// resolution and failure classification all belong to the agent.
package exec

import (
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jonasohland/mxl-replicator/internal/logging"
	"github.com/jonasohland/mxl-replicator/internal/worker"
)

const (
	// DefaultBinary is the worker's name, unchanged from the legacy proxy on purpose: it makes
	// it obvious at a glance which process belongs to which layer, and which one is covered by
	// docs/worker-runtime-surface.md (§2.2).
	DefaultBinary = "mxl-replicator-worker"

	// DefaultWorkRoot is where per-start work directories live. A tmpfs under /run, because
	// nothing here survives a reboot and nothing here should (§6.1).
	DefaultWorkRoot = "/run/mxl-replicator"

	// DefaultStopGrace is how long a worker gets to exit on SIGTERM before it is killed. The
	// legacy supervisor used the same value; the worker notices a signal within 500 ms in its
	// receive and connect loops and 1000 ms in the initiator's drain (WRS §5.4), so this is
	// generous rather than tight.
	DefaultStopGrace = 5 * time.Second
)

const (
	workDirPrefix     = "w-"
	configName        = "config.json"
	metricsSocketName = "metrics.sock"
	targetInfoName    = "target-info.json"

	// sunPathMax is the size of sockaddr_un.sun_path, including its NUL terminator.
	//
	// The worker now fails with a clear ENAMETOOLONG rather than silently truncating into it,
	// which is what used to make two workers under a long parent directory bind *the same*
	// path and let the second die with EADDRINUSE naming a socket it never configured
	// (WRS §6). Checking here as well turns that into a startup error naming the configured
	// root, which is the thing an operator can actually fix.
	sunPathMax = 108
)

// Options configures a [Launcher].
type Options struct {
	// Binary is the worker executable, looked up on PATH when it is not a path. Defaults to
	// [DefaultBinary].
	Binary string

	// WorkRoot is the parent of every per-start work directory. Defaults to
	// [DefaultWorkRoot]. Created if it does not exist.
	WorkRoot string

	// LogLevel is rendered into MXL_LOG_LEVEL for the worker's environment (§12).
	//
	// The worker's only environment knob, and the legacy supervisor never set it — which left
	// every spdlog::debug call in the transfer loops compiled in but permanently silent.
	LogLevel slog.Level

	// Env is extra environment for the worker, on top of the agent's own. libfabric takes a
	// good deal of its configuration this way (FI_*), and a deployment that needs to steer it
	// has nowhere else to put that.
	Env map[string]string

	// StopGrace is how long a worker gets to exit on SIGTERM before SIGKILL. Defaults to
	// [DefaultStopGrace].
	StopGrace time.Duration

	// Logger receives the launcher's own messages and the workers' translated output. Required.
	Logger *slog.Logger
}

// Launcher starts real worker processes. Safe for concurrent use.
type Launcher struct {
	binary    string
	workRoot  string
	env       []string
	stopGrace time.Duration
	log       *slog.Logger
}

var _ worker.Launcher = (*Launcher)(nil)

// NewLauncher validates the options and prepares the work root.
//
// It deliberately does *not* probe the binary. That is the agent's `-v` load probe (§10.5,
// [ProbeVersions]), which happens once at startup and reports versions the server needs
// anyway; doing it again here would be a second answer to the same question.
func NewLauncher(opts Options) (*Launcher, error) {
	if opts.Logger == nil {
		return nil, fmt.Errorf("worker: no logger")
	}

	launcher := &Launcher{
		binary:    orDefault(opts.Binary, DefaultBinary),
		workRoot:  orDefault(opts.WorkRoot, DefaultWorkRoot),
		stopGrace: opts.StopGrace,
		log:       opts.Logger,
	}
	if launcher.stopGrace <= 0 {
		launcher.stopGrace = DefaultStopGrace
	}

	if !filepath.IsAbs(launcher.workRoot) {
		return nil, fmt.Errorf("worker: work root %q is not absolute", launcher.workRoot)
	}
	if budget := maxWorkRootLen(); len(launcher.workRoot) > budget {
		return nil, fmt.Errorf(
			"worker: work root %q is %d bytes, leaving no room for a metrics socket path under the %d-byte sun_path limit (max %d)",
			launcher.workRoot, len(launcher.workRoot), sunPathMax, budget)
	}
	if err := os.MkdirAll(launcher.workRoot, 0o700); err != nil {
		return nil, fmt.Errorf("worker: create work root: %w", err)
	}

	launcher.env = buildEnv(opts)
	return launcher, nil
}

// maxWorkRootLen is the longest work root that still leaves room for
// "<root>/w-<10 digits>/metrics.sock" plus the NUL sun_path counts.
func maxWorkRootLen() int {
	const maxRandomDigits = 10
	name := len(workDirPrefix) + maxRandomDigits
	return sunPathMax - 1 - len("/") - name - len("/") - len(metricsSocketName)
}

func buildEnv(opts Options) []string {
	env := os.Environ()
	for key, value := range opts.Env {
		env = append(env, key+"="+value)
	}
	// Last wins in execve, so the agent's level is authoritative over anything inherited.
	env = append(env, "MXL_LOG_LEVEL="+logging.WorkerLogLevel(opts.LogLevel))
	return env
}

func orDefault(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}

// Sweep removes work directories left behind by a previous agent process.
//
// The agent holds no persistent state and does not adopt workers: a restart kills and
// re-establishes everything on the node (§6.1). So anything under the work root at startup is
// the debris of a process that is gone, and leaving it costs disk and makes the directory
// unreadable when something does need looking at.
//
// Call this once, before starting anything. It assumes the work root belongs to this agent
// alone, which node-name exclusivity already guarantees (§7.1) — two agents sharing a root
// would be two agents claiming one node.
func Sweep(root string, log *slog.Logger) error {
	entries, err := os.ReadDir(root)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("worker: sweep %s: %w", root, err)
	}

	var errs []error
	for _, entry := range entries {
		if !entry.IsDir() || !strings.HasPrefix(entry.Name(), workDirPrefix) {
			continue
		}
		path := filepath.Join(root, entry.Name())
		if err := os.RemoveAll(path); err != nil {
			errs = append(errs, err)
			continue
		}
		if log != nil {
			log.Debug("removed stale worker directory", "path", path)
		}
	}
	return errors.Join(errs...)
}
