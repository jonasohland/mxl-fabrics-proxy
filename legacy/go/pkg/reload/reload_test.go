package reload

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jonasohland/mxl-fabrics-proxy/legacy/go/pkg/server"
	"github.com/stretchr/testify/require"
)

func write(t *testing.T, path string, contents string) {
	t.Helper()
	require.NoError(t, os.WriteFile(path, []byte(contents), 0600))
}

func TestFingerprintIsStable(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "a.yaml"), "defaults: {}\n")
	write(t, filepath.Join(dir, "b.yml"), "domains: {}\n")

	require.Equal(t, Fingerprint([]string{dir}), Fingerprint([]string{dir}))
}

func TestFingerprintChangesWithContents(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "a.yaml")

	write(t, path, "defaults: {}\n")
	before := Fingerprint([]string{dir})

	write(t, path, "defaults: {node: 10.0.0.1}\n")
	require.NotEqual(t, before, Fingerprint([]string{dir}))
}

func TestFingerprintChangesWhenFileIsAdded(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "a.yaml"), "defaults: {}\n")
	before := Fingerprint([]string{dir})

	write(t, filepath.Join(dir, "b.yaml"), "defaults: {}\n")
	require.NotEqual(t, before, Fingerprint([]string{dir}))
}

func TestFingerprintChangesWhenFileIsRemoved(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "a.yaml")
	write(t, path, "defaults: {}\n")
	write(t, filepath.Join(dir, "b.yaml"), "domains: {}\n")
	before := Fingerprint([]string{dir})

	require.NoError(t, os.Remove(path))
	require.NotEqual(t, before, Fingerprint([]string{dir}))
}

// Files the proxy would not load must not trigger a reload.
func TestFingerprintIgnoresOtherFiles(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "a.yaml"), "defaults: {}\n")
	before := Fingerprint([]string{dir})

	write(t, filepath.Join(dir, "notes.txt"), "hello\n")
	require.Equal(t, before, Fingerprint([]string{dir}))
}

// A config volume that is not mounted yet must not be fatal, and its appearance
// must register as a change.
func TestFingerprintMissingPath(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "not-there")

	before := Fingerprint([]string{missing})

	require.NoError(t, os.Mkdir(missing, 0700))
	write(t, filepath.Join(missing, "a.yaml"), "defaults: {}\n")

	require.NotEqual(t, before, Fingerprint([]string{missing}))
}

// An empty file must be distinguishable from a file that cannot be read.
func TestFingerprintEmptyFileIsNotUnreadable(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "a.yaml")
	write(t, path, "")

	empty := Fingerprint([]string{path})

	require.NoError(t, os.Remove(path))
	require.NotEqual(t, empty, Fingerprint([]string{path}))
}

// runWatcher starts a watcher against a stub proxy and returns the channel the
// stub reports reload requests on, as "METHOD PATH".
func runWatcher(t *testing.T, paths []string) chan string {
	t.Helper()

	requests := make(chan string, 8)
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests <- r.Method + " " + r.URL.Path
		server.Response(w, &server.Message{Message: "configuration reloaded", Code: http.StatusOK})
	}))
	t.Cleanup(stub.Close)

	watcher, err := NewWatcher(Options{
		Paths:    paths,
		Server:   stub.URL,
		Interval: 10 * time.Millisecond,
		Debounce: 20 * time.Millisecond,
		Timeout:  5 * time.Second,
	})
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		watcher.Run(ctx)
	}()

	t.Cleanup(func() {
		cancel()
		<-done
	})

	return requests
}

func requireReload(t *testing.T, requests chan string) {
	t.Helper()

	select {
	case request := <-requests:
		require.Equal(t, "POST /v1/reload", request)
	case <-time.After(10 * time.Second):
		t.Fatal("expected a reload request")
	}
}

func requireNoReload(t *testing.T, requests chan string) {
	t.Helper()

	select {
	case request := <-requests:
		t.Fatalf("unexpected reload request: %s", request)
	case <-time.After(200 * time.Millisecond):
	}
}

// The proxy may have loaded its configuration before the current contents were
// in place, so the reloader must not assume the two are in sync.
func TestWatcherReloadsOnStartup(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "a.yaml"), "defaults: {}\n")

	requests := runWatcher(t, []string{dir})

	requireReload(t, requests)
	requireNoReload(t, requests)
}

func TestWatcherReloadsOnChange(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "a.yaml")
	write(t, path, "defaults: {}\n")

	requests := runWatcher(t, []string{dir})
	requireReload(t, requests)

	write(t, path, "defaults: {node: 10.0.0.1}\n")

	requireReload(t, requests)
	requireNoReload(t, requests)
}

func TestReloadURL(t *testing.T) {
	for _, test := range []struct {
		server   string
		expected string
	}{
		{"127.0.0.1:2283", "http://127.0.0.1:2283/v1/reload"},
		{"localhost", "http://localhost/v1/reload"},
		{"http://proxy.my.org:2283", "http://proxy.my.org:2283/v1/reload"},
		{"http://proxy.my.org:2283/", "http://proxy.my.org:2283/v1/reload"},
		{"https://proxy.my.org", "https://proxy.my.org/v1/reload"},
		{"[::1]:2283", "http://[::1]:2283/v1/reload"},
	} {
		actual, err := reloadURL(test.server)
		require.NoError(t, err, test.server)
		require.Equal(t, test.expected, actual)
	}
}

func TestReloadURLInvalid(t *testing.T) {
	for _, server := range []string{"", "http://", "://nope"} {
		_, err := reloadURL(server)
		require.ErrorIs(t, err, ErrInvalidOptions, server)
	}
}
