package metrics

import (
	"context"
	"math"
	"net"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jonasohland/mxl-replicator/internal/worker"
)

func TestParseLine(t *testing.T) {
	t.Parallel()

	counter, ok := ParseLine("mxl_grains_total 300")
	require.True(t, ok)
	assert.Equal(t, worker.Counter("mxl_grains_total", 300), counter)

	summary, ok := ParseLine("mxl_source_latency_ns[0.5] 498000")
	require.True(t, ok)
	require.True(t, summary.IsSummary())
	assert.Equal(t, "mxl_source_latency_ns", summary.Name)
	assert.InDelta(t, 0.5, *summary.Quantile, 0)
	assert.InDelta(t, 498000, summary.Value, 0)
}

// A summary with no observations in its sliding window emits nan, which means "nothing
// measured" — not zero. Turning it into 0 would report a latency quantile that never happened.
func TestParseLineKeepsNaN(t *testing.T) {
	t.Parallel()

	sample, ok := ParseLine("mxl_network_latency_ns[0.99] nan")
	require.True(t, ok)
	assert.True(t, math.IsNaN(sample.Value))
}

func TestParseLineRejects(t *testing.T) {
	t.Parallel()

	for name, line := range map[string]string{
		"empty":              "",
		"no value":           "mxl_grains_total",
		"non-numeric":        "mxl_grains_total lots",
		"no name":            " 300",
		"unclosed quantile":  "mxl_source_latency_ns[0.5 498000",
		"bad quantile":       "mxl_source_latency_ns[half] 498000",
		"no name before [":   "[0.5] 498000",
		"prometheus comment": "# TYPE mxl_grains_total counter",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, ok := ParseLine(line)
			assert.False(t, ok)
		})
	}
}

// serve stands in for the worker's metrics socket: accept, write a snapshot, close. The client
// sends nothing and reads to EOF (WRS §6).
func serve(t *testing.T, body string) string {
	t.Helper()

	socket := filepath.Join(t.TempDir(), "metrics.sock")
	listener, err := net.Listen("unix", socket)
	require.NoError(t, err)
	t.Cleanup(func() { _ = listener.Close() })

	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			_, _ = conn.Write([]byte(body))
			_ = conn.Close()
		}
	}()
	return socket
}

func TestScrape(t *testing.T) {
	t.Parallel()

	socket := serve(t, "mxl_octets_total 1234567\nmxl_grains_total 300\nmxl_source_latency_ns[0.5] 498000\n")

	samples, err := Scrape(t.Context(), socket)
	require.NoError(t, err)
	require.Len(t, samples, 3)
	assert.Equal(t, "mxl_octets_total", samples[0].Name)
	assert.True(t, samples[2].IsSummary())
}

// An unrecognised line is not a reason to lose the metrics that came with it.
func TestScrapeSkipsUnparseableLines(t *testing.T) {
	t.Parallel()

	socket := serve(t, "garbage\nmxl_grains_total 300\n\nalso garbage\n")

	samples, err := Scrape(t.Context(), socket)
	require.NoError(t, err)
	require.Len(t, samples, 1)
	assert.Equal(t, "mxl_grains_total", samples[0].Name)
}

func TestScrapeFailsOnAMissingSocket(t *testing.T) {
	t.Parallel()

	_, err := Scrape(t.Context(), filepath.Join(t.TempDir(), "nope.sock"))
	assert.Error(t, err)
}

// The scrape is a read to EOF with no framing of its own, so cancellation has to come from
// closing the connection underneath it — a worker that accepts and then says nothing must not
// hold a scrape open indefinitely.
func TestScrapeHonoursTheCallersDeadline(t *testing.T) {
	t.Parallel()

	socket := filepath.Join(t.TempDir(), "metrics.sock")
	listener, err := net.Listen("unix", socket)
	require.NoError(t, err)
	t.Cleanup(func() { _ = listener.Close() })

	accepted := make(chan net.Conn, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		accepted <- conn // held open, never written to
	}()

	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()

	_, err = Scrape(ctx, socket)
	require.ErrorIs(t, err, context.DeadlineExceeded)

	select {
	case conn := <-accepted:
		_ = conn.Close()
	case <-time.After(time.Second):
	}
}
