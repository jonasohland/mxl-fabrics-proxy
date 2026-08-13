package health_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/jonasohland/mxl-fabrics-proxy/go/pkg/health"
	"github.com/jonasohland/mxl-fabrics-proxy/go/pkg/initiator"
	"github.com/jonasohland/mxl-fabrics-proxy/go/pkg/metrics"
	"github.com/jonasohland/mxl-fabrics-proxy/go/pkg/target"
	"github.com/jonasohland/mxl-fabrics-proxy/go/pkg/worker"
	"github.com/stretchr/testify/require"
)

func get(t *testing.T, mux *http.ServeMux, path string) *http.Response {
	t.Helper()

	recorder := httptest.NewRecorder()
	mux.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))

	return recorder.Result()
}

func TestHealth(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	wg := &sync.WaitGroup{}
	t.Cleanup(func() {
		cancel()
		wg.Wait()
	})

	m := metrics.NewMetrics()
	domains := initiator.NewDomains()
	subs := initiator.NewSubscriptions(ctx, wg, domains, m, false)
	targets := target.NewTargets(ctx, m)

	_, err := domains.Add("mxl:///dev/shm/mxl0", "mxl0", "127.0.0.1", "tcp")
	require.NoError(t, err)

	mux := http.NewServeMux()
	health.NewHealth(worker.Versions{Proxy: "0.0.1", MXL: "1.1.0", Libfabric: "2.6"},
		domains, subs, targets).Mux(mux)

	for _, path := range []string{"/healthz", "/v1/health"} {
		res := get(t, mux, path)
		require.Equal(t, http.StatusOK, res.StatusCode, path)

		var status health.Status
		require.NoError(t, json.NewDecoder(res.Body).Decode(&status), path)

		require.Equal(t, "ok", status.Status)
		require.Equal(t, "0.0.1", status.ProxyVersion)
		require.Equal(t, "1.1.0", status.MXLVersion)
		require.Equal(t, "2.6", status.LibfabricVersion)
		require.Equal(t, 1, status.Domains)
		require.Equal(t, 0, status.Subscriptions)
		require.Equal(t, 0, status.Targets)
		require.Positive(t, status.UptimeSeconds)
	}
}
