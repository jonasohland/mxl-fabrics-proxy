package metrics

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/jonasohland/mxl-fabrics-proxy/legacy/go/pkg/worker"
	"github.com/samber/lo"
)

type Metrics struct {
	mu      sync.Mutex
	workers []*worker.ProxyWorker
}

func NewMetrics() *Metrics {
	return &Metrics{mu: sync.Mutex{}, workers: nil}
}

func (m *Metrics) Mux(mux *http.ServeMux) {
	mux.Handle("GET /metrics", m)
}

func sendLabels(w io.Writer, labels map[string]string) error {
	if _, err := io.WriteString(w, "{"); err != nil {
		return err
	}

	i := 0
	for name, label := range labels {
		labelBuf, err := json.Marshal(&label)
		if err != nil {
			continue
		}

		if _, err := io.WriteString(w, name+"="+string(labelBuf)); err != nil {
			return err
		}
		if i != len(labels)-1 {
			if _, err := io.WriteString(w, ","); err != nil {
				return err
			}
		}
		i++
	}

	if _, err := io.WriteString(w, "}"); err != nil {
		return err
	}

	return nil
}

func sendCounterMetricLine(w io.Writer, name string, labels map[string]string, v float64) error {
	if _, err := io.WriteString(w, name); err != nil {
		return err
	}

	if err := sendLabels(w, labels); err != nil {
		return err
	}

	if _, err := io.WriteString(w, fmt.Sprintf(" %f\n", v)); err != nil {
		return err
	}

	return nil
}

func sendCounterMetric(w io.Writer, name string, labels map[string]string, v float64) error {
	if _, err := io.WriteString(w, fmt.Sprintf("# TYPE %s counter\n", name)); err != nil {
		return err
	}

	return sendCounterMetricLine(w, name, labels, v)
}

func sendSummaryMetric(w io.Writer, name string, labels map[string]string, summary map[float64]float64) error {
	if _, err := io.WriteString(w, fmt.Sprintf("# TYPE %s summary\n", name)); err != nil {
		return err
	}

	for q, v := range summary {
		labels["quantile"] = strconv.FormatFloat(q, 'f', 3, 64)
		if err := sendCounterMetricLine(w, name, labels, v); err != nil {
			return err
		}
	}

	return nil
}

func (m *Metrics) Add(worker *worker.ProxyWorker) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if lo.Contains(m.workers, worker) {
		return
	}

	m.workers = append(m.workers, worker)
}

func (m *Metrics) Remove(worker *worker.ProxyWorker) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.workers = lo.Without(m.workers, worker)
}

func (m *Metrics) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	m.mu.Lock()
	defer m.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	wg := sync.WaitGroup{}
	wg.Add(len(m.workers))

	collected := make(chan any)
	collect := make(chan *worker.MetricCollection)
	for _, worker := range m.workers {
		go func() {
			defer wg.Done()
			metrics, err := worker.GetMetrics(ctx)
			if err != nil {
				slog.Error("scrape error", "error", err)
				collect <- nil
			}
			collect <- metrics
		}()
	}

	buf := bytes.NewBuffer(nil)
	go func() {
		defer close(collected)
		for {
			metrics, ok := <-collect
			if !ok {
				return
			}

			if metrics != nil {
				for name, metric := range metrics.Counters {
					if err := sendCounterMetric(buf, name, metrics.Labels, metric); err != nil {
						slog.Error("error", "err", err)
						return
					}
				}
				for name, metric := range metrics.Summaries {
					if err := sendSummaryMetric(buf, name, metrics.Labels, metric); err != nil {
						slog.Error("error", "err", err)
						return
					}
				}
			}
		}
	}()
	wg.Wait()
	close(collect)
	<-collected

	w.Header().Set("Content-Length", strconv.Itoa(buf.Len()))
	if _, err := w.Write(buf.Bytes()); err != nil {
		slog.Error("err", "error", err)
	}
}
