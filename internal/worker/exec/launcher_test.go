package exec

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jonasohland/mxl-replicator/internal/api"
	"github.com/jonasohland/mxl-replicator/internal/worker"
)

// syncBuffer collects log output from the launcher's goroutines.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func newLauncher(t *testing.T, env map[string]string) (*Launcher, *syncBuffer) {
	t.Helper()
	// Generous, because the worker here is a race-instrumented Go test binary rather than the
	// real thing. Only the kill path wants a short one.
	return newLauncherWithGrace(t, env, 5*time.Second)
}

func newLauncherWithGrace(t *testing.T, env map[string]string, grace time.Duration) (*Launcher, *syncBuffer) {
	t.Helper()

	logs := &syncBuffer{}
	full := map[string]string{helperEnv: "1"}
	for k, v := range env {
		full[k] = v
	}

	launcher, err := NewLauncher(Options{
		Binary:    os.Args[0],
		WorkRoot:  t.TempDir(),
		LogLevel:  slog.LevelDebug,
		Env:       full,
		StopGrace: grace,
		Logger:    slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
	})
	require.NoError(t, err)
	return launcher, logs
}

// waitReady blocks until the worker has logged "connected", past which it has its signal
// disposition installed. Deterministic where a sleep would be a race: a process signalled
// before it reaches that point dies of the default disposition, which is a fact about process
// startup rather than anything the launcher does.
func waitReady(t *testing.T, logs *syncBuffer) {
	t.Helper()
	require.Eventually(t, func() bool {
		return strings.Contains(logs.String(), "connected")
	}, 10*time.Second, 10*time.Millisecond, "worker never came up")
}

func targetSpec() worker.Spec {
	return worker.Spec{
		SessionID:   "s-1",
		Role:        api.RoleTarget,
		DomainPath:  "/dev/shm/mxl1",
		FlowID:      "5592a23b-0974-45bb-9388-89ea81c42537",
		FlowDef:     json.RawMessage(`{"id":"5592a23b-0974-45bb-9388-89ea81c42537"}`),
		BindAddress: "10.0.2.4",
		Service:     "24012",
		Interface: api.InterfaceConfig{
			Provider:       api.ProviderTCP,
			CapFlags:       []api.CapFlag{api.CapRemoteWrite, api.CapBlockingOperations},
			MaxMessageSize: 1048576,
		},
	}
}

func initiatorSpec() worker.Spec {
	spec := targetSpec()
	spec.Role = api.RoleInitiator
	spec.Epoch = "NONCE:abcd"
	spec.DomainPath = "/dev/shm/mxl0"
	spec.FlowDef = nil
	spec.TargetInfo = helperTargetInfo
	spec.ConnectTimeout = 60 * time.Second
	return spec
}

func start(t *testing.T, l *Launcher, spec worker.Spec) *handle {
	t.Helper()
	h, err := l.Start(t.Context(), spec)
	require.NoError(t, err)
	t.Cleanup(func() { _ = h.Stop(context.Background()) })
	return h.(*handle)
}

func TestStartWritesTheConfigTheWorkerReads(t *testing.T) {
	launcher, _ := newLauncher(t, nil)
	h := start(t, launcher, initiatorSpec())

	raw, err := os.ReadFile(filepath.Join(h.dir, configName))
	require.NoError(t, err)

	var cfg map[string]any
	require.NoError(t, json.Unmarshal(raw, &cfg))

	assert.Equal(t, false, cfg["target"])
	assert.Equal(t, "/dev/shm/mxl0", cfg["domain"])
	assert.Equal(t, "10.0.2.4", cfg["node"])
	assert.Equal(t, "24012", cfg["service"])
	assert.Equal(t, "tcp", cfg["provider"])
	assert.Equal(t, helperTargetInfo, cfg["target_info"], "an initiator gets the blob inline")
	assert.Equal(t, filepath.Join(h.dir, metricsSocketName), cfg["metrics_socket"])

	// The sentinels: present and zero, not absent. An omitted idle_timeout_ms is the worker's
	// built-in 10 s, which is the opposite of what "wait indefinitely" asks for (§11.1).
	require.Contains(t, cfg, "idle_timeout_ms")
	assert.Equal(t, float64(0), cfg["idle_timeout_ms"])
	assert.Equal(t, float64(60000), cfg["connect_timeout_ms"])

	// Keys no version of the worker has ever read (WRS §3).
	for _, dead := range []string{"proxy_id", "efa_use_wait", "labels"} {
		assert.NotContains(t, cfg, dead)
	}
}

func TestTargetConfigCarriesTheFlowDefinitionAndAnOutputPath(t *testing.T) {
	launcher, _ := newLauncher(t, map[string]string{helperNoTargetInfoEnv: "1"})
	h := start(t, launcher, targetSpec())

	raw, err := os.ReadFile(filepath.Join(h.dir, configName))
	require.NoError(t, err)

	var cfg map[string]any
	require.NoError(t, json.Unmarshal(raw, &cfg))

	assert.Equal(t, true, cfg["target"])
	assert.Equal(t, filepath.Join(h.dir, targetInfoName), cfg["target_info"],
		"a target's target_info is an output path, not a blob")
	assert.Equal(t, `{"id":"5592a23b-0974-45bb-9388-89ea81c42537"}`, cfg["flow_def"],
		"the definition travels as JSON inside a JSON string")
}

func TestTargetInfoArrives(t *testing.T) {
	launcher, _ := newLauncher(t, map[string]string{helperInfoDelayEnv: "50"})
	h := start(t, launcher, targetSpec())

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	blob, err := h.TargetInfo(ctx)
	require.NoError(t, err)
	assert.Equal(t, helperTargetInfo, blob)
}

// A Create event can arrive with nothing behind it, because the worker writes the file with an
// ordinary buffered stream rather than an atomic rename (WRS §5.1). Handing the agent half a
// blob would fail to decode later and look like a corrupt target rather than a race.
func TestTargetInfoWaitsForACompleteBlob(t *testing.T) {
	launcher, _ := newLauncher(t, map[string]string{helperPartialInfoEnv: "1"})
	h := start(t, launcher, targetSpec())

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	blob, err := h.TargetInfo(ctx)
	require.NoError(t, err)
	assert.Equal(t, helperTargetInfo, blob)
}

func TestTargetInfoHonoursTheCallersDeadline(t *testing.T) {
	launcher, _ := newLauncher(t, map[string]string{helperNoTargetInfoEnv: "1"})
	h := start(t, launcher, targetSpec())

	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()

	_, err := h.TargetInfo(ctx)
	assert.ErrorIs(t, err, context.DeadlineExceeded)
}

// A bad domain kills a target before its metrics socket exists, let alone its blob (WRS §5.1),
// so the wait has to end when the process does.
func TestTargetInfoEndsWhenTheWorkerDies(t *testing.T) {
	launcher, _ := newLauncher(t, map[string]string{helperExitEnv: "1"})
	h := start(t, launcher, targetSpec())

	_, err := h.TargetInfo(t.Context())
	assert.ErrorIs(t, err, worker.ErrExited)
}

func TestTargetInfoRefusesAnInitiator(t *testing.T) {
	launcher, _ := newLauncher(t, nil)
	h := start(t, launcher, initiatorSpec())

	_, err := h.TargetInfo(t.Context())
	assert.ErrorIs(t, err, worker.ErrNotTarget)
}

func TestMetricsScrape(t *testing.T) {
	launcher, _ := newLauncher(t, map[string]string{
		helperMetricsEnv: "mxl_grains_total 300\nmxl_source_latency_ns[0.5] 498000\nmxl_network_latency_ns[0.99] nan\n",
	})
	h := start(t, launcher, initiatorSpec())

	var samples []worker.Sample
	require.Eventually(t, func() bool {
		got, err := h.Metrics(t.Context())
		if err != nil {
			return false
		}
		samples = got
		return len(samples) == 3
	}, 10*time.Second, 20*time.Millisecond)

	assert.Equal(t, worker.Counter("mxl_grains_total", 300), samples[0])
	require.True(t, samples[1].IsSummary())
	assert.InDelta(t, 0.5, *samples[1].Quantile, 0)
	assert.True(t, samples[2].Value != samples[2].Value, "a summary with no observations reports nan, which is not zero")
}

func TestUnexpectedExitIsReportedAsUnexpected(t *testing.T) {
	launcher, _ := newLauncher(t, map[string]string{helperExitEnv: "1"})
	h := start(t, launcher, initiatorSpec())

	select {
	case exit := <-h.Exited():
		assert.False(t, exit.Stopped, "the agent did not ask for this one")
		assert.Error(t, exit.Err)
		assert.False(t, exit.At.IsZero())
	case <-time.After(10 * time.Second):
		t.Fatal("no exit reported")
	}

	_, err := h.Metrics(t.Context())
	assert.ErrorIs(t, err, worker.ErrExited)
}

func TestStopTerminatesCleanly(t *testing.T) {
	launcher, logs := newLauncher(t, nil)
	h := start(t, launcher, initiatorSpec())
	waitReady(t, logs)

	require.NoError(t, h.Stop(t.Context()))

	exit := <-h.Exited()
	assert.True(t, exit.Stopped)
	assert.NoError(t, exit.Err, "the fake worker exits 0 on SIGTERM, as the real one does")
	assert.NoDirExists(t, h.dir)
}

func TestStopKillsAWorkerThatIgnoresSigterm(t *testing.T) {
	launcher, logs := newLauncherWithGrace(t, map[string]string{helperIgnoreSigtermEnv: "1"}, 300*time.Millisecond)
	h := start(t, launcher, initiatorSpec())
	waitReady(t, logs)

	require.NoError(t, h.Stop(t.Context()))

	exit := <-h.Exited()
	assert.True(t, exit.Stopped)
	assert.Contains(t, logs.String(), "did not exit on SIGTERM")
	assert.NoDirExists(t, h.dir)
}

// Stopping a worker that already died is the normal call: the agent stops a dead handle before
// starting its replacement, and that is what finally removes the directory.
func TestStopIsIdempotentAndWorksOnADeadWorker(t *testing.T) {
	launcher, _ := newLauncher(t, map[string]string{helperExitEnv: "3"})
	h := start(t, launcher, initiatorSpec())

	<-h.Exited()
	assert.DirExists(t, h.dir, "a failing worker's config survives until it is stopped")

	require.NoError(t, h.Stop(t.Context()))
	require.NoError(t, h.Stop(t.Context()))
	assert.NoDirExists(t, h.dir)
}

func TestEachStartGetsAFreshDirectory(t *testing.T) {
	launcher, _ := newLauncher(t, map[string]string{helperNoTargetInfoEnv: "1"})

	first := start(t, launcher, targetSpec())
	second := start(t, launcher, targetSpec())

	assert.NotEqual(t, first.dir, second.dir)
	assert.FileExists(t, filepath.Join(first.dir, configName))
	assert.FileExists(t, filepath.Join(second.dir, configName))
}

func TestWorkerOutputIsTranslated(t *testing.T) {
	launcher, logs := newLauncher(t, nil)
	start(t, launcher, initiatorSpec())
	waitReady(t, logs)

	out := logs.String()
	assert.Contains(t, out, `module=worker`)
	assert.Contains(t, out, "session=s-1")
	// MXL_LOG_LEVEL is the worker's only environment knob, and the legacy supervisor never set
	// it — leaving every spdlog::debug call in the transfer loops permanently silent (§12).
	assert.Contains(t, out, "MXL_LOG_LEVEL=debug")
}

func TestSweepRemovesOnlyWorkDirectories(t *testing.T) {
	root := t.TempDir()
	stale := filepath.Join(root, workDirPrefix+"123")
	require.NoError(t, os.MkdirAll(stale, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(stale, configName), []byte("{}"), 0o600))

	keepDir := filepath.Join(root, "not-ours")
	require.NoError(t, os.MkdirAll(keepDir, 0o700))
	keepFile := filepath.Join(root, workDirPrefix+"file")
	require.NoError(t, os.WriteFile(keepFile, []byte("x"), 0o600))

	require.NoError(t, Sweep(root, nil))

	assert.NoDirExists(t, stale)
	assert.DirExists(t, keepDir)
	assert.FileExists(t, keepFile)
}

func TestSweepIsFineWithAMissingRoot(t *testing.T) {
	assert.NoError(t, Sweep(filepath.Join(t.TempDir(), "nope"), nil))
}

func TestNewLauncherRejectsAnUnusableWorkRoot(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(&syncBuffer{}, nil))

	_, err := NewLauncher(Options{WorkRoot: "run/mxl", Logger: logger})
	assert.ErrorContains(t, err, "not absolute")

	// The sun_path budget, checked at startup so that a deployment which cannot work says so
	// then rather than at its first assignment (WRS §6).
	_, err = NewLauncher(Options{WorkRoot: "/" + strings.Repeat("d", 120), Logger: logger})
	assert.ErrorContains(t, err, "sun_path")

	_, err = NewLauncher(Options{WorkRoot: t.TempDir()})
	assert.ErrorContains(t, err, "logger")
}

func TestStartRejectsAnInvalidSpec(t *testing.T) {
	launcher, _ := newLauncher(t, nil)

	spec := initiatorSpec()
	spec.Epoch = ""

	_, err := launcher.Start(t.Context(), spec)
	require.Error(t, err)

	entries, err := os.ReadDir(launcher.workRoot)
	require.NoError(t, err)
	assert.Empty(t, entries, "a refused start leaves nothing behind")
}
