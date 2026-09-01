package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	osexec "os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/jonasohland/mxl-utils/pkg/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jonasohland/mxl-replicator/internal/agent"
	"github.com/jonasohland/mxl-replicator/internal/agent/probe"
	"github.com/jonasohland/mxl-replicator/internal/api"
	"github.com/jonasohland/mxl-replicator/internal/worker/exec"
)

// M7.11: the real thing. The actual worker binary, the actual MXL library, the actual libfabric,
// on one host with no special hardware — the equivalent of the legacy loopback.yml (§17).
//
// Everything else in this package fakes the worker, which is right: it makes the control plane
// testable anywhere and it is the control plane that the other ten cases are about. What it
// cannot do is tell you whether a worker pair actually moves media. That is a different claim,
// and the only way to make it is to run them.
//
// # Why the payload is verified rather than counted
//
// A session can move data and still be wrong. The initiator computes scatter-gather offsets
// *within* the bounce buffer ring from entrySize/entryCount, so a stale value writes into a
// correctly registered region at the wrong offsets: the NIC reports nothing, the destination
// unpacks garbage, and every counter in the system reads healthy (§5.2). "The head index
// advanced" cannot catch that, and neither can a byte total. mxl-mock-src fills each grain from a
// pure function of (seed, index) and mxl-mock-sink checks it, so a grain shifted by one word — or
// arriving at the wrong index — fails as loudly as a corrupted one.
//
// # Why one node
//
// shm is structurally same-node-only: its fabric label is derived from the node name, so two
// different node names can never pair over it (§10.1). One agent with two mapped domains
// replicating between them is therefore the *only* shape a shm test can have — and it is exactly
// what the legacy loopback configuration does. The tcp case runs in the same shape so that the
// two differ in one variable rather than two.
//
// Skips when the binaries are not built, following the same rule as the etcd tests: a machine
// that has not built the C++ side should still be able to run `go test ./...`.
func TestLoopbackOverSHM(t *testing.T) {
	runLoopback(t, api.ProviderSHM, probe.Attachment{Provider: api.ProviderSHM})
}

func TestLoopbackOverTCP(t *testing.T) {
	runLoopback(t, api.ProviderTCP, probe.Attachment{
		Provider: api.ProviderTCP,
		Fabric:   "loopback",
		Address:  "127.0.0.1",
	})
}

// grainsToVerify is how many grains the sink must read and check byte-for-byte. At 24000/1001
// that is a little under a second of video, which is long enough to be past establishment and
// short enough that a failing run reports promptly.
const grainsToVerify = 20

func runLoopback(t *testing.T, provider api.Provider, attachment probe.Attachment) {
	workerBinary := locate(t, "mxl-fabrics-proxy-worker", "MXL_REPLICATOR_TEST_WORKER_BINARY")
	producer := locate(t, "mxl-mock-src", "MXL_REPLICATOR_TEST_MOCK_SRC")
	consumer := locate(t, "mxl-mock-sink", "MXL_REPLICATOR_TEST_MOCK_SINK")

	// Registered before the fleet, so it is torn down after it: cleanups run last-registered
	// first, and removing a domain out from under a running worker is not a case this test is
	// trying to exercise.
	domainRoot := shmDir(t)

	launcher, workRoot := realLauncher(t, workerBinary)

	f := newFleet(t, fleetOptions{})
	node := f.addNode("loopback", nodeOptions{
		// "src" only: the destination is an output domain, materialised under this node's output
		// root when the target assignment is accepted (§10.6).
		domains:    []string{"src"},
		domainRoot: domainRoot,
		launcher:   launcher,
		probe:      realProbe(t, workerBinary, "loopback", attachment),
		tweak: func(cfg *agent.Config) {
			// Real processes, not instant ones. The rest of the suite's timings assume a fake
			// launcher that starts a struct.
			cfg.TargetInfoTimeout = 30 * time.Second
			cfg.BackoffMin = 200 * time.Millisecond
			cfg.BackoffMax = 2 * time.Second
			cfg.StopGrace = 10 * time.Second
		},
	})

	// The producer writes into the source domain the agent just created. It runs until the test
	// stops it, so the flow keeps being produced for as long as anything is looking at it.
	definition := writeFlowDefinition(t, "E2E Camera 1")
	source := start(t, producer,
		"--domain", node.path("src"),
		"--flow-def", definition.path,
		"--seed", "7",
		"--json")

	f.eventually("the source flow to be observed and producing", func() bool {
		for _, flow := range f.flows().Flows {
			if flow.ID == definition.id && flow.Domain == node.sourceName("src") {
				return flow.Producing
			}
		}
		return false
	})

	// Pinned, not preferred. The point of running this twice is to exercise two providers, and a
	// pin is honoured or the request fails — it is never substituted (§10.4), so a tcp run that
	// quietly landed on shm would report a pass for something it did not test.
	request := f.request(api.RequestSpec{
		Name:         "loopback-" + string(provider),
		Sources:      []api.Source{{Node: "loopback", Domain: node.source("src"), Select: api.Selector{Flow: definition.id}}},
		Destinations: []api.Destination{{Node: "loopback", Domain: api.Domain{Area: "fast", Elements: []string{"dst"}}}},
		Provider:     api.ProviderPin{provider},
		Labels:       map[string]string{"suite": "e2e"},
	})

	f.eventually("the path to go ACTIVE", func() bool {
		paths := f.paths().Paths
		if len(paths) != 1 {
			return false
		}
		if state := paths[0].State; state == api.StateInvalid || state == api.StateFailed {
			t.Fatalf("path %s: %s (%s)", state, paths[0].Reason, paths[0].ReasonCode)
		}
		return paths[0].State == api.StateActive
	})

	// The negotiated result is the one that was asked for, on both ends.
	path := f.onlyPath()
	require.NotNil(t, path.Session)
	assert.Equal(t, provider, path.Session.Interface.Provider)
	require.NotNil(t, path.Session.Target)
	require.NotNil(t, path.Session.Initiator)
	assert.Equal(t, api.WorkerReady, path.Session.Target.State)
	assert.Equal(t, api.WorkerReady, path.Session.Initiator.State)
	assert.Zero(t, path.Session.Target.Restarts, "a healthy session establishes once")
	assert.Zero(t, path.Session.Initiator.Restarts)

	// The claim this whole test exists to make: the bytes at the destination are the bytes the
	// producer wrote, at the indices it wrote them to.
	sink := run(t, 90*time.Second, consumer,
		"--domain", node.path("dst"),
		"--flow-id", definition.id,
		"--verify",
		"--seed", "7",
		"--count", fmt.Sprint(grainsToVerify),
		"--wait-for-flow", "30000",
		"--idle-timeout", "30000",
		"--json")

	var summary struct {
		OK         bool   `json:"ok"`
		Read       int    `json:"read"`
		Verified   int    `json:"verified"`
		Mismatched int    `json:"mismatched"`
		Gaps       int    `json:"gaps"`
		Failure    string `json:"failure"`
	}
	require.NoError(t, json.Unmarshal([]byte(sink.stdout), &summary), "sink stdout: %s", sink.stdout)

	assert.Zero(t, sink.code, "mxl-mock-sink failed: %s\n%s", summary.Failure, sink.stderr)
	assert.True(t, summary.OK, "sink reported failure: %s", summary.Failure)
	assert.Equal(t, grainsToVerify, summary.Read)
	assert.Equal(t, grainsToVerify, summary.Verified, "every grain read must verify against the pattern")
	assert.Zero(t, summary.Mismatched, "a mismatch is a grain that arrived at the wrong offset or corrupted")
	assert.Zero(t, summary.Gaps, "a gap is a grain that never arrived")

	// Cancelling the request stops both workers. Real processes this time, so the assertion is
	// that they are gone rather than that a struct was marked dead.
	f.cancel(request.RequestID())
	f.eventually("the path to be withdrawn", func() bool {
		return len(f.paths().Paths) == 0
	})
	f.eventually("both worker processes to exit", func() bool {
		return countWorkers(t, workerBinary, workRoot) == 0
	})

	source.stop()
}

// --- the real launcher and probe -------------------------------------------------------------

// realLauncher builds the launcher that execs worker processes, and returns the work root it was
// given — which is unique to this test and is therefore how its workers are told apart from
// anything else on the host.
func realLauncher(t *testing.T, binary string) (*exec.Launcher, string) {
	t.Helper()

	// A short work root on purpose: it holds an AF_UNIX socket path, and the launcher refuses a
	// root that leaves no room under the 108-byte sun_path limit (WRS §6).
	root := filepath.Join(t.TempDir(), "w")
	require.NoError(t, os.MkdirAll(root, 0o700))

	launcher, err := exec.NewLauncher(exec.Options{
		Binary:   binary,
		WorkRoot: root,
		LogLevel: slog.LevelWarn,
		Logger:   discard(),
	})
	require.NoError(t, err)
	return launcher, root
}

// realProbe is §10.5's join, run for real: what libfabric reports on this host, intersected with
// what the operator declared. An attachment that is configured and not reported is dropped, which
// is what makes an advertised capability mean "verified" rather than "hoped for" (§10.2).
func realProbe(t *testing.T, binary, node string, attachment probe.Attachment) func(context.Context) (api.Capabilities, error) {
	t.Helper()
	require.NoError(t, attachment.Validate())

	return func(ctx context.Context) (api.Capabilities, error) {
		versions, err := exec.ProbeVersions(ctx, binary)
		if err != nil {
			return api.Capabilities{}, err
		}
		interfaces, err := exec.ProbeInterfaces(ctx, binary)
		if err != nil {
			return api.Capabilities{}, err
		}

		joined := probe.Join([]probe.Attachment{attachment}, interfaces, probe.Options{
			Node:   node,
			Logger: discard(),
		})
		if len(joined.Attachments) == 0 {
			// Reported as a probe failure rather than as an empty registration, because a node
			// that advertises nothing fails every negotiation on the *other* node with
			// no_shared_fabric, which is a long way from the mistake.
			return api.Capabilities{}, fmt.Errorf(
				"libfabric reports no interface matching %s on this host", attachment)
		}
		return api.Capabilities{Fabrics: joined.Attachments, Versions: versions}, nil
	}
}

// --- flow definitions ---------------------------------------------------------------------------

// flowDefinition is a file on disk plus the ID inside it.
type flowDefinition struct {
	id   string
	path string
}

// writeFlowDefinition writes the NMOS flow definition mxl-mock-src creates its flow from.
//
// Written out as a literal rather than by marshalling one of mxl-utils' types: this file is what
// a *producer* writes, so it is part of the input to the test rather than something the test
// shares an encoder with. (mxl.Rational also has no JSON tags, and would serialise its fields
// capitalised, which the library does not accept.)
func writeFlowDefinition(t *testing.T, groupName string) flowDefinition {
	t.Helper()

	// A real UUID per run, so a leftover flow directory from an interrupted run cannot be
	// mistaken for this one's.
	id := testutil.NewVideoFlowDef(testutil.FlowSize1080, testutil.FlowRate23).ID

	body := map[string]any{
		"id":          id,
		"label":       "e2e " + groupName,
		"description": "mxl-replicator end-to-end test flow",
		"format":      "urn:x-nmos:format:video",
		"media_type":  "video/v210",
		// The library requires a group hint and refuses to create a flow without one.
		"tags":           map[string][]string{"urn:x-nmos:tag:grouphint/v1.0": {groupName + ":video"}},
		"interlace_mode": "progressive",
		"frame_width":    1920,
		"frame_height":   1080,
		"grain_rate":     map[string]int{"numerator": 24000, "denominator": 1001},
	}

	encoded, err := json.MarshalIndent(body, "", "  ")
	require.NoError(t, err)

	path := filepath.Join(t.TempDir(), "flow-def.json")
	require.NoError(t, os.WriteFile(path, encoded, 0o600))
	return flowDefinition{id: id, path: path}
}

// --- processes ------------------------------------------------------------------------------

// locate finds one of the C++ binaries: an explicit override, then PATH, then this project's own
// build directory, which is where `make` puts them.
func locate(t *testing.T, name, env string) string {
	t.Helper()

	if override := os.Getenv(env); override != "" {
		return override
	}
	if found, err := osexec.LookPath(name); err == nil {
		return found
	}

	built, err := filepath.Abs(filepath.Join("..", "..", "build", name))
	if err == nil {
		if info, err := os.Stat(built); err == nil && !info.IsDir() {
			return built
		}
	}

	t.Skipf("%s is not on PATH, not in ./build, and %s is not set; run `make cmake-build`", name, env)
	return ""
}

// shmDir returns a directory under /dev/shm for this test's MXL domains, removed afterwards.
//
// /dev/shm rather than t.TempDir() because that is where an MXL domain lives: the flows in it are
// shared memory, and a domain on a disk-backed filesystem is a different thing being tested.
func shmDir(t *testing.T) string {
	t.Helper()

	// The name has to be short and unique. t.Name() carries slashes for subtests and is long
	// enough to matter, so the process ID and the test's own temp directory basename do the work.
	root := filepath.Join("/dev/shm", "mxlrepl-e2e-"+filepath.Base(t.TempDir()))
	require.NoError(t, os.MkdirAll(root, 0o755))
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	return root
}

// process is one of the mock tools running in the background.
type process struct {
	t   *testing.T
	cmd *osexec.Cmd

	stdout, stderr *lockedBuffer
	done           chan error
	once           sync.Once
}

// start runs a tool in the background, terminated when the test ends.
func start(t *testing.T, name string, args ...string) *process {
	t.Helper()

	p := &process{
		t:      t,
		cmd:    osexec.Command(name, args...),
		stdout: &lockedBuffer{},
		stderr: &lockedBuffer{},
		done:   make(chan error, 1),
	}
	p.cmd.Stdout, p.cmd.Stderr = p.stdout, p.stderr
	require.NoError(t, p.cmd.Start(), "start %s", name)

	go func() { p.done <- p.cmd.Wait() }()
	t.Cleanup(p.stop)
	return p
}

// stop asks the process to finish and waits for it. SIGTERM rather than SIGKILL, because these
// tools report a summary on the way out and a killed one reports nothing. Idempotent.
func (p *process) stop() {
	p.once.Do(func() {
		_ = p.cmd.Process.Signal(syscall.SIGTERM)
		select {
		case <-p.done:
		case <-time.After(10 * time.Second):
			_ = p.cmd.Process.Kill()
			<-p.done
			p.t.Errorf("%s did not exit on SIGTERM", filepath.Base(p.cmd.Path))
		}
	})
}

// result is a finished process.
type result struct {
	code           int
	stdout, stderr string
}

// run executes a tool to completion.
//
// stdout carries the machine-readable summary and stderr everything else, which is a split the
// tools enforce on purpose (WRS §2): the MXL library installs a logger writing to stdout on first
// use, so a summary a test parses would otherwise arrive interleaved with libfabric diagnostics.
func run(t *testing.T, timeout time.Duration, name string, args ...string) result {
	t.Helper()

	ctx, cancel := context.WithTimeout(t.Context(), timeout)
	defer cancel()

	cmd := osexec.CommandContext(ctx, name, args...)
	var stdout, stderr lockedBuffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr

	err := cmd.Run()
	if ctx.Err() != nil {
		t.Fatalf("%s did not finish within %s\n%s", filepath.Base(name), timeout, stderr.String())
	}

	var exit *osexec.ExitError
	if err != nil && !errors.As(err, &exit) {
		t.Fatalf("%s: %v\n%s", filepath.Base(name), err, stderr.String())
	}
	return result{code: cmd.ProcessState.ExitCode(), stdout: stdout.String(), stderr: stderr.String()}
}

// countWorkers reports how many worker processes this test's agent still has running.
//
// Matched on the work root, which is this test's own temp directory and appears on every worker's
// command line as the path to its config file. A leaked worker holds a port, a memory
// registration and a flow, so "the path was withdrawn" is not the same claim as "the processes
// are gone" and is not a substitute for it.
func countWorkers(t *testing.T, binary, workRoot string) int {
	t.Helper()

	entries, err := os.ReadDir("/proc")
	require.NoError(t, err)

	base := filepath.Base(binary)
	count := 0
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		raw, err := os.ReadFile(filepath.Join("/proc", entry.Name(), "cmdline"))
		if err != nil {
			continue // exited between the listing and the read, which is the answer we wanted
		}
		line := strings.ReplaceAll(string(raw), "\x00", " ")
		if strings.Contains(line, base) && strings.Contains(line, workRoot) {
			count++
		}
	}
	return count
}

// lockedBuffer collects a subprocess's output while it runs, for a failing test to print.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
