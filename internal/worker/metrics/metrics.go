// Package metrics reads a worker's metrics socket (WRS §6).
//
// The protocol is deliberately trivial: connect, send nothing, read to EOF. The worker
// snapshots every metric on accept and closes the connection when the buffer drains, so one
// connection is one point-in-time scrape.
//
// The output is line-oriented text with no labels and no TYPE lines — the worker emits
// neither, and labelling is the agent's job (§12).
package metrics

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"strconv"
	"strings"

	"github.com/jonasohland/mxl-replicator/internal/worker"
)

// Scrape connects to a worker's metrics socket and reads one snapshot.
//
// Unparseable lines are skipped rather than failing the scrape: a metric this build does not
// recognise is not a reason to lose the ones it does.
func Scrape(ctx context.Context, socket string) ([]worker.Sample, error) {
	var dialer net.Dialer
	conn, err := dialer.DialContext(ctx, "unix", socket)
	if err != nil {
		return nil, fmt.Errorf("metrics: dial %s: %w", socket, err)
	}
	defer func() { _ = conn.Close() }()

	// The scrape is a read to EOF with no application-level framing, so cancellation has to
	// come from closing the connection underneath it.
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-done:
		case <-ctx.Done():
			_ = conn.Close()
		}
	}()

	var samples []worker.Sample
	scanner := bufio.NewScanner(conn)
	for scanner.Scan() {
		if sample, ok := ParseLine(scanner.Text()); ok {
			samples = append(samples, sample)
		}
	}
	if err := scanner.Err(); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		return nil, fmt.Errorf("metrics: read %s: %w", socket, err)
	}
	return samples, nil
}

// ParseLine parses one line of the worker's metrics output.
//
// Counters are `name value`; summary quantiles are `name[quantile] value`.
//
// A value of `nan` parses to NaN and is kept: a summary with no observations in its sliding
// window emits exactly that, and it means "nothing measured", which is not zero.
func ParseLine(line string) (worker.Sample, bool) {
	name, value, found := strings.Cut(strings.TrimSpace(line), " ")
	if !found || name == "" {
		return worker.Sample{}, false
	}

	parsed, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
	if err != nil {
		return worker.Sample{}, false
	}

	base, quantile, isSummary := strings.Cut(name, "[")
	if !isSummary {
		return worker.Counter(name, parsed), true
	}
	if base == "" || !strings.HasSuffix(quantile, "]") {
		return worker.Sample{}, false
	}
	q, err := strconv.ParseFloat(strings.TrimSuffix(quantile, "]"), 64)
	if err != nil {
		return worker.Sample{}, false
	}
	return worker.Quantile(base, q, parsed), true
}
