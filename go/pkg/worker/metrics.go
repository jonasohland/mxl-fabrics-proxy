package worker

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"strconv"
	"strings"

	"github.com/samber/lo"
)

type MetricCollection struct {
	Counters  map[string]float64
	Summaries map[string]map[float64]float64
	Labels    map[string]string
}

func (m *MetricCollection) AddQuant(label string, q, v float64) {
	if m.Summaries == nil {
		m.Summaries = make(map[string]map[float64]float64)
	}
	if m.Summaries[label] == nil {
		m.Summaries[label] = make(map[float64]float64)
	}

	m.Summaries[label][q] = v
}

func (m *MetricCollection) AddCounter(label string, v float64) {
	if m.Counters == nil {
		m.Counters = make(map[string]float64)
	}

	m.Counters[label] = v
}

func (m *MetricCollection) Parse(line string) {
	parts := strings.SplitN(line, " ", 2)
	if len(parts) != 2 {
		return
	}

	v, err := strconv.ParseFloat(parts[1], 64)
	if err != nil {
		return
	}

	parts2 := strings.SplitN(strings.TrimSuffix(parts[0], "]"), "[", 2)
	if len(parts2) == 1 {
		m.AddCounter(parts[0], v)
	}
	if len(parts2) == 2 {
		q, err := strconv.ParseFloat(parts2[1], 64)
		if err != nil {
			return
		}

		m.AddQuant(parts2[0], q, v)
	}
}

func (w *ProxyWorker) GetMetrics(ctx context.Context) (*MetricCollection, error) {
	socketPath := w.GetMetricsSocket()
	conn, err := (&net.Dialer{}).DialContext(ctx, "unix", socketPath)
	if err != nil {
		return nil, fmt.Errorf("connect to metrics socket: %w", err)
	}

	done := make(chan any)
	defer close(done)
	go func() {
		select {
		case <-done:
		case <-ctx.Done():
			_ = conn.Close()
		}
	}()

	coll := &MetricCollection{
		Summaries: nil,
		Counters:  nil,
		Labels: map[string]string{
			"direction": lo.Ternary(w.config.IsInitiator(), "initiator", "target"),
			"domain":    w.config.Domain,
			"flowID":    w.config.FlowID,
		},
	}
	coll.Labels = lo.Assign(coll.Labels, w.config.Labels)

	scanner := bufio.NewScanner(conn)
	for scanner.Scan() {
		coll.Parse(scanner.Text())
	}
	if scanner.Err() != nil {
		return nil, scanner.Err()
	}

	coll.AddCounter("workerRestarts", float64(w.NumRestarts()))
	return coll, nil
}
