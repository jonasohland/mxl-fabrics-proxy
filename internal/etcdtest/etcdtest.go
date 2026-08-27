// Package etcdtest starts a real etcd for tests.
//
// It lives outside the packages that use it because two of them need it — the store backend and
// leader election — and both need it for the same reason: they exist to inherit behaviour from
// etcd, and a fake would inherit that behaviour from this project's beliefs about etcd instead.
// A conformance suite or an election test passing against a mirror proves nothing.
package etcdtest

import (
	"bytes"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// Callers run against a **real** etcd, started as a subprocess. The store backend exists
// precisely to inherit behaviour from etcd — when a no-op delete declines to advance the
// revision, when a watcher is actually established, what a keepalive against a dead lease
// answers — and leader election inherits even more of it.
//
// etcd is not vendored for this. It is looked up on PATH and the tests skip without it, which
// keeps `go test ./...` working on a machine that has not got one — the trade being that a CI
// job which never installs etcd will report these as skipped rather than failed. Point
// MXL_REPLICATOR_TEST_ETCD_ENDPOINTS at a running cluster to use that instead.
const endpointsEnv = "MXL_REPLICATOR_TEST_ETCD_ENDPOINTS"

// Start brings up a single-node etcd for the duration of one top-level test.
//
// One server per top-level test, not per store: startup is a second or so, and the conformance
// suite opens twenty stores. Sharing across *top-level* tests is avoided on purpose, because a
// lease leaked by one test expiring during another would advance the shared store revision
// under a case that asserts a revision did not move.
func Start(t *testing.T) []string {
	t.Helper()

	if eps := os.Getenv(endpointsEnv); eps != "" {
		return strings.Split(eps, ",")
	}

	bin, err := exec.LookPath("etcd")
	if err != nil {
		t.Skipf("etcd is not on PATH and %s is not set (%v)", endpointsEnv, err)
	}

	// Called before the cleanup below is registered, so the directory is removed after the
	// process that is using it has been reaped: t.Cleanup runs last-registered-first.
	dir := t.TempDir()

	clientURL := fmt.Sprintf("http://127.0.0.1:%d", FreePort(t))
	peerURL := fmt.Sprintf("http://127.0.0.1:%d", FreePort(t))

	cmd := exec.Command(bin,
		"--name", "test",
		"--data-dir", filepath.Join(dir, "data"),
		"--listen-client-urls", clientURL,
		"--advertise-client-urls", clientURL,
		"--listen-peer-urls", peerURL,
		"--initial-advertise-peer-urls", peerURL,
		"--initial-cluster", "test="+peerURL,
		"--initial-cluster-state", "new",
		"--initial-cluster-token", "mxl-replicator-"+t.Name(),
		"--log-level", "error",
		// Test data, and fsync per write makes the lease and watch cases noticeably slower.
		"--unsafe-no-fsync",
	)

	var out lockedBuffer
	cmd.Stdout, cmd.Stderr = &out, &out
	require.NoError(t, cmd.Start(), "start etcd")

	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()

	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		<-exited
		if t.Failed() {
			t.Logf("etcd output:\n%s", out.String())
		}
	})

	waitHealthy(t, clientURL, exited, &out)
	return []string{clientURL}
}

// waitHealthy blocks until etcd answers /health, or the test fails.
func waitHealthy(t *testing.T, clientURL string, exited <-chan error, out *lockedBuffer) {
	t.Helper()

	deadline := time.Now().Add(30 * time.Second)
	for {
		select {
		case err := <-exited:
			t.Fatalf("etcd exited before becoming healthy: %v\n%s", err, out.String())
		default:
		}

		if time.Now().After(deadline) {
			t.Fatalf("etcd did not become healthy within 30s\n%s", out.String())
		}

		resp, err := http.Get(clientURL + "/health") //nolint:noctx // bounded by the deadline above
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// FreePort returns a port nothing is listening on. Inherently a guess — something else could
// take it before etcd binds — but it is the only way to run a server on a port a test chose.
func FreePort(t *testing.T) int {
	t.Helper()

	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer l.Close() //nolint:errcheck // the point is to release the port

	return l.Addr().(*net.TCPAddr).Port
}

// lockedBuffer collects the server's output while it runs, for a failing test to print.
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
