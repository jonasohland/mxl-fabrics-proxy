package reload

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/jonasohland/mxl-fabrics-proxy/go/pkg/server"
)

const (
	backoffBase = time.Second
	backoffMax  = 30 * time.Second
)

const noFingerprint = ""

var ErrInvalidOptions = errors.New("invalid options")

type Options struct {
	Paths    []string
	Server   string
	Interval time.Duration
	Debounce time.Duration
	Timeout  time.Duration
}

type Watcher struct {
	log     *slog.Logger
	client  *http.Client
	options Options
	url     string

	applied string
	seen    string
	seenAt  time.Time

	failures  int
	nextRetry time.Time
}

func reloadURL(server string) (string, error) {
	if !strings.Contains(server, "://") {
		server = "http://" + server
	}

	surl, err := url.Parse(server)
	if err != nil {
		return "", fmt.Errorf("%w: parse server address: %w", ErrInvalidOptions, err)
	}

	if surl.Host == "" {
		return "", fmt.Errorf("%w: server address '%s' has no host", ErrInvalidOptions, server)
	}

	surl.Path = strings.TrimSuffix(surl.Path, "/") + "/v1/reload"
	return surl.String(), nil
}

func NewWatcher(options Options) (*Watcher, error) {
	if len(options.Paths) == 0 {
		return nil, fmt.Errorf("%w: no configuration paths given", ErrInvalidOptions)
	}
	if options.Interval <= 0 {
		return nil, fmt.Errorf("%w: interval must be positive", ErrInvalidOptions)
	}
	if options.Debounce < 0 {
		return nil, fmt.Errorf("%w: debounce must not be negative", ErrInvalidOptions)
	}
	if options.Timeout <= 0 {
		return nil, fmt.Errorf("%w: timeout must be positive", ErrInvalidOptions)
	}

	rurl, err := reloadURL(options.Server)
	if err != nil {
		return nil, err
	}

	return &Watcher{
		log:     slog.With("module", "reload"),
		client:  &http.Client{},
		options: options,
		url:     rurl,
	}, nil
}

// Run polls the configuration until the context is cancelled.
//
// A reload is always triggered once at startup: the proxy loads its
// configuration by itself, but there is no ordering guarantee between the two
// processes, so it may have read the files before they were in place. The
// debounce applies to this first reload as well, which also gives a config
// volume that is still being populated time to settle.
func (w *Watcher) Run(ctx context.Context) {
	w.applied = noFingerprint
	w.seen = Fingerprint(w.options.Paths)
	w.seenAt = time.Now()

	w.log.Info("watching configuration",
		"paths", w.options.Paths, "url", w.url, "fingerprint", short(w.seen))

	ticker := time.NewTicker(w.options.Interval)
	defer ticker.Stop()

	for {
		w.poll(ctx, time.Now())

		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (w *Watcher) poll(ctx context.Context, now time.Time) {
	current := Fingerprint(w.options.Paths)

	if current != w.seen {
		w.log.Info("configuration changed", "fingerprint", short(current))

		w.seen = current
		w.seenAt = now

		w.failures = 0
		w.nextRetry = time.Time{}
	}

	if w.seen == w.applied {
		return
	}
	if now.Sub(w.seenAt) < w.options.Debounce {
		return
	}
	if now.Before(w.nextRetry) {
		return
	}

	applying := w.seen
	if err := w.reload(ctx); err != nil {
		if ctx.Err() != nil {
			return
		}

		w.failures++
		backoff := w.backoff()
		w.nextRetry = time.Now().Add(backoff)

		w.log.Error("reload failed", "error", err, "attempt", w.failures, "retry-in", backoff)
		return
	}

	w.log.Info("configuration reloaded", "fingerprint", short(applying))

	w.applied = applying
	w.failures = 0
	w.nextRetry = time.Time{}
}

func (w *Watcher) backoff() time.Duration {
	shift := min(max(w.failures-1, 0), 5)
	return min(backoffBase<<shift, backoffMax)
}

func (w *Watcher) reload(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, w.options.Timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, w.url, nil)
	if err != nil {
		return err
	}

	res, err := w.client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = res.Body.Close() }()

	if res.StatusCode != http.StatusOK {
		var msg server.Message
		if err := json.NewDecoder(io.LimitReader(res.Body, 1<<16)).Decode(&msg); err == nil && msg.Message != "" {
			return fmt.Errorf("%s: %s", res.Status, msg.Message)
		}

		return errors.New(res.Status)
	}

	_, _ = io.Copy(io.Discard, io.LimitReader(res.Body, 1<<16))
	return nil
}
