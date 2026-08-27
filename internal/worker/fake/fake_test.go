package fake

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jonasohland/mxl-replicator/internal/api"
	"github.com/jonasohland/mxl-replicator/internal/epoch"
	"github.com/jonasohland/mxl-replicator/internal/worker"
)

func targetSpec(session string) worker.Spec {
	return worker.Spec{
		SessionID:   session,
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

func initiatorSpec(session, blob, ep string) worker.Spec {
	return worker.Spec{
		SessionID:   session,
		Role:        api.RoleInitiator,
		Epoch:       ep,
		DomainPath:  "/dev/shm/mxl0",
		FlowID:      "5592a23b-0974-45bb-9388-89ea81c42537",
		BindAddress: "10.0.1.7",
		Service:     "24011",
		Interface: api.InterfaceConfig{
			Provider:       api.ProviderTCP,
			CapFlags:       []api.CapFlag{api.CapRemoteWrite, api.CapBlockingOperations},
			MaxMessageSize: 1048576,
		},
		TargetInfo: blob,
	}
}

func start(t *testing.T, l *Launcher, spec worker.Spec) *Handle {
	t.Helper()
	h, err := l.Start(t.Context(), spec)
	require.NoError(t, err)
	return h.(*Handle)
}

func TestStartRecordsEverySpec(t *testing.T) {
	t.Parallel()

	l := New()
	start(t, l, targetSpec("s-1"))
	start(t, l, targetSpec("s-2"))

	specs := l.Starts()
	require.Len(t, specs, 2)
	assert.Equal(t, "s-1", specs[0].SessionID)
	assert.Equal(t, "s-2", specs[1].SessionID)
	assert.Equal(t, 2, l.StartCount())
	assert.Len(t, l.Running(), 2)
	assert.NotNil(t, l.Find("s-1", api.RoleTarget))
	assert.Nil(t, l.Find("s-1", api.RoleInitiator))
}

// The fake validates for the same reason a real launcher does: a control-plane test that builds
// a nonsensical assignment should fail in that test, not in a restart loop that reads like a
// fabric problem.
func TestStartValidatesTheSpec(t *testing.T) {
	t.Parallel()

	l := New()
	spec := targetSpec("s-1")
	spec.DomainPath = ""

	_, err := l.Start(t.Context(), spec)
	require.Error(t, err)
	assert.Empty(t, l.Running())
}

func TestFailNextStartIsOneShotAndRecorded(t *testing.T) {
	t.Parallel()

	l := New()
	boom := errors.New("no such binary")
	l.FailNextStart(boom)

	_, err := l.Start(t.Context(), targetSpec("s-1"))
	require.ErrorIs(t, err, boom)
	assert.Len(t, l.Starts(), 1, "a refused start is still an attempt")
	assert.Empty(t, l.Running())

	start(t, l, targetSpec("s-1"))
	assert.Len(t, l.Running(), 1)
}

// The blob has to be a real one, or nothing downstream of it is a real test: the agent decodes
// it, hashes it and reports the result as an epoch (§5.2, §5.3).
func TestDefaultTargetInfoIsDecodable(t *testing.T) {
	t.Parallel()

	l := New()
	h := start(t, l, targetSpec("s-1"))

	blob, err := h.TargetInfo(t.Context())
	require.NoError(t, err)

	info, unknown, err := epoch.Decode(blob)
	require.NoError(t, err)
	assert.Empty(t, unknown, "the fake must not invent fields the real library does not report")
	assert.Equal(t, "tcp", info.Provider)
	assert.Equal(t, "10.0.2.4:24012", info.FabricAddress)
	require.Len(t, info.Regions, 1)
	assert.Equal(t, epoch.U64(0), info.Regions[0].Addr, "the tcp provider reports no mapping address")
}

func TestDefaultTargetInfoDiffersPerStart(t *testing.T) {
	t.Parallel()

	l := New()
	first := start(t, l, targetSpec("s-1"))
	blobA, err := first.TargetInfo(t.Context())
	require.NoError(t, err)
	require.NoError(t, first.Stop(t.Context()))

	second := start(t, l, targetSpec("s-1"))
	blobB, err := second.TargetInfo(t.Context())
	require.NoError(t, err)

	assert.NotEqual(t, blobA, blobB)
}

// The degenerate case the incarnation nonce exists for, wired end to end through the fake: a
// target that restarts and reports a **byte-identical** blob still produces a new epoch, so its
// initiator reconnects rather than running happily against rkeys that no longer exist (§5.2).
// Without a pinnable blob here, no test in M7 can reach this.
func TestPinnedTargetInfoIsByteIdenticalButStillANewEpoch(t *testing.T) {
	t.Parallel()

	l := New()
	l.SetTargetInfo(TargetInfo(targetSpec("s-1"), 1))

	first := start(t, l, targetSpec("s-1"))
	blobA, err := first.TargetInfo(t.Context())
	require.NoError(t, err)
	require.NoError(t, first.Stop(t.Context()))

	second := start(t, l, targetSpec("s-1"))
	blobB, err := second.TargetInfo(t.Context())
	require.NoError(t, err)

	require.Equal(t, blobA, blobB)

	infoA, _, err := epoch.Decode(blobA)
	require.NoError(t, err)
	infoB, _, err := epoch.Decode(blobB)
	require.NoError(t, err)

	assert.NotEqual(t, epoch.Compute(epoch.NewNonce(), infoA), epoch.Compute(epoch.NewNonce(), infoB))
}

func TestSetTargetInfoFunc(t *testing.T) {
	t.Parallel()

	l := New()
	l.SetTargetInfoFunc(func(spec worker.Spec, seq int) string {
		return fmt.Sprintf(`{"id":"%s","seq":%d}`, spec.SessionID, seq)
	})

	h := start(t, l, targetSpec("s-7"))
	blob, err := h.TargetInfo(t.Context())
	require.NoError(t, err)
	assert.Equal(t, `{"id":"s-7","seq":1}`, blob)
}

func TestTargetInfoRefusesAnInitiator(t *testing.T) {
	t.Parallel()

	l := New()
	h := start(t, l, initiatorSpec("s-1", `{"id":"1001"}`, "NONCE:abcd"))

	_, err := h.TargetInfo(t.Context())
	assert.ErrorIs(t, err, worker.ErrNotTarget)
}

func TestHeldTargetInfoBlocksUntilReleased(t *testing.T) {
	t.Parallel()

	l := New()
	l.HoldTargetInfo(true)
	h := start(t, l, targetSpec("s-1"))

	blobs := make(chan string, 1)
	go func() {
		blob, err := h.TargetInfo(context.Background())
		if err == nil {
			blobs <- blob
		}
	}()

	select {
	case <-blobs:
		t.Fatal("target info was available while held")
	case <-time.After(20 * time.Millisecond):
	}

	l.HoldTargetInfo(false)
	select {
	case blob := <-blobs:
		assert.NotEmpty(t, blob)
	case <-time.After(time.Second):
		t.Fatal("target info did not arrive after release")
	}
}

func TestHeldTargetInfoHonoursTheCallersDeadline(t *testing.T) {
	t.Parallel()

	l := New()
	l.HoldTargetInfo(true)
	h := start(t, l, targetSpec("s-1"))

	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()

	_, err := h.TargetInfo(ctx)
	assert.ErrorIs(t, err, context.DeadlineExceeded)
}

// A bad domain path kills a target before its metrics socket exists, let alone its blob
// (WRS §5.1), so waiting for target info has to end when the process does.
func TestTargetInfoEndsWhenTheWorkerDies(t *testing.T) {
	t.Parallel()

	l := New()
	l.HoldTargetInfo(true)
	h := start(t, l, targetSpec("s-1"))

	go func() {
		time.Sleep(10 * time.Millisecond)
		h.Die(errors.New("exit status 1"))
	}()

	_, err := h.TargetInfo(t.Context())
	assert.ErrorIs(t, err, worker.ErrExited)
}

func TestDieDeliversAnUnexpectedExit(t *testing.T) {
	t.Parallel()

	l := New()
	h := start(t, l, targetSpec("s-1"))

	boom := errors.New("exit status 1")
	h.Die(boom)

	exit := <-h.Exited()
	assert.False(t, exit.Stopped, "nobody asked for this one")
	assert.ErrorIs(t, exit.Err, boom)
	assert.False(t, exit.At.IsZero())
	assert.False(t, h.Running())
	assert.Empty(t, l.Running())
}

// The worker self-terminating on its idle timeout is a clean exit nobody asked for, and the
// agent tells the two apart by whether it sent the signal — not by the exit status (§15.1).
func TestCleanDeathIsStillUnexpected(t *testing.T) {
	t.Parallel()

	l := New()
	h := start(t, l, targetSpec("s-1"))
	h.Die(nil)

	exit := <-h.Exited()
	assert.False(t, exit.Stopped)
	assert.NoError(t, exit.Err)
}

func TestStopIsIdempotent(t *testing.T) {
	t.Parallel()

	l := New()
	h := start(t, l, targetSpec("s-1"))

	require.NoError(t, h.Stop(t.Context()))
	require.NoError(t, h.Stop(t.Context()))

	exit := <-h.Exited()
	assert.True(t, exit.Stopped)
	assert.False(t, h.Running())
	assert.Equal(t, 2, h.Stops())
}

func TestStopAfterDeathKeepsTheOriginalExit(t *testing.T) {
	t.Parallel()

	l := New()
	h := start(t, l, targetSpec("s-1"))

	boom := errors.New("exit status 1")
	h.Die(boom)
	require.NoError(t, h.Stop(t.Context()))

	exit := <-h.Exited()
	assert.False(t, exit.Stopped, "an exit the agent did not ask for must not be relabelled by the cleanup that follows")
	assert.ErrorIs(t, exit.Err, boom)
}

func TestMetrics(t *testing.T) {
	t.Parallel()

	l := New()
	l.SetMetrics([]worker.Sample{
		worker.Counter("mxl_grains_total", 300),
		worker.Quantile("mxl_source_latency_ns", 0.5, 498000),
	})
	h := start(t, l, targetSpec("s-1"))

	samples, err := h.Metrics(t.Context())
	require.NoError(t, err)
	require.Len(t, samples, 2)
	assert.False(t, samples[0].IsSummary())
	require.True(t, samples[1].IsSummary())
	assert.InDelta(t, 0.5, *samples[1].Quantile, 0)

	require.NoError(t, h.Stop(t.Context()))
	_, err = h.Metrics(t.Context())
	assert.ErrorIs(t, err, worker.ErrExited, "a dead worker has no socket to scrape")
}

func TestFindPanicsOnDuplicateWorkers(t *testing.T) {
	t.Parallel()

	l := New()
	start(t, l, targetSpec("s-1"))
	start(t, l, targetSpec("s-1"))

	assert.Panics(t, func() { l.Find("s-1", api.RoleTarget) })
}
