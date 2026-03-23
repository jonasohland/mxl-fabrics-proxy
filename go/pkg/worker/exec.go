package worker

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/samber/lo"
)

var (
	ErrInvalidVersionOutput = errors.New("invalid version output from proxy worker")
)

type Versions struct {
	Proxy     string
	MXL       string
	Libfabric string
}

func GetVersions() (Versions, error) {
	versions := Versions{}
	buf := bytes.NewBuffer(nil)
	cmd := exec.Command("mxl-fabrics-proxy-worker", "-v")
	cmd.Stderr = buf

	if err := cmd.Run(); err != nil {
		return Versions{}, err
	}

	for line := range strings.Lines(buf.String()) {
		elms := strings.SplitN(line, " ", 2)
		if len(elms) != 2 {
			return versions, ErrInvalidVersionOutput
		}

		v := strings.Trim(elms[1], " \n")
		switch elms[0] {
		case "proxy":
			versions.Proxy = v
		case "mxl":
			versions.MXL = v
		case "libfabric":
			versions.Libfabric = v
		}
	}

	return versions, nil
}

type ProxyWorker struct {
	mu  sync.Mutex
	wg  sync.WaitGroup
	log *slog.Logger

	config   Config
	workdir  string
	restarts atomic.Uint64

	ctx        context.Context
	cancel     context.CancelFunc
	terminated chan any
}

func NewWorker(config Config) *ProxyWorker {
	return &ProxyWorker{
		mu:  sync.Mutex{},
		wg:  sync.WaitGroup{},
		log: slog.With("proxy-id", config.ProxyID, "module", "proxy"),

		config:   config,
		workdir:  "",
		restarts: atomic.Uint64{},

		ctx:        nil,
		cancel:     nil,
		terminated: nil,
	}
}

func (w *ProxyWorker) Start(ctx context.Context, wg *sync.WaitGroup) {
	wg.Add(1)
	go w.run(ctx, wg)
}

func (w *ProxyWorker) GetTargetInfo() (string, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	buf, err := os.ReadFile(filepath.Join(w.workdir, "target-info.json"))
	if err != nil {
		return "", err
	}

	return string(buf), nil
}

func (w *ProxyWorker) GetMetricsSocket() string {
	w.mu.Lock()
	defer w.mu.Unlock()

	return w.config.MetricsSocket
}

func (w *ProxyWorker) NumRestarts() uint64 {
	return w.restarts.Load()
}

func (w *ProxyWorker) run(ctx context.Context, wg *sync.WaitGroup) {
	wait := time.Duration(0)
	defer wg.Done()
	for {
		select {
		case <-time.After(wait):
		case <-ctx.Done():
			return
		}

		wait = 3 * time.Second

		if err := w.restart(); err != nil {
			w.log.Error("failed to restart proxy worker", "error", err)
		}

		select {
		case <-ctx.Done():
			w.terminate()
			return
		case <-w.terminated:
			w.restarts.Add(1)
			w.terminate()
		}
	}

}

func (w *ProxyWorker) restart() error {
	w.mu.Lock()
	defer w.mu.Unlock()

	w.log.Info("restarting proxy worker")

	workdir, err := os.MkdirTemp(os.TempDir(), "mxl-fabrics-proxy-worker-")
	if err != nil {
		return err
	}
	if w.config.IsTarget() {
		w.config.TargetInfo = filepath.Join(workdir, "target-info.json")
	}
	if w.config.Service == "" {
		w.config.Service = strconv.Itoa(rand.Intn(20000) + 20000)
	}

	w.config.MetricsSocket = filepath.Join(workdir, "metrics.sock")

	fd, err := os.OpenFile(filepath.Join(workdir, "config.json"), os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer func() { _ = fd.Close() }()

	if err := json.NewEncoder(fd).Encode(&w.config); err != nil {
		return err
	}

	ctx, cancel := context.WithCancel(context.Background())

	cmd := exec.CommandContext(ctx, "mxl-fabrics-proxy-worker", filepath.Join(workdir, "config.json"))
	cmd.WaitDelay = 5 * time.Second
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		return err
	}

	w.ctx = ctx
	w.cancel = cancel
	w.workdir = workdir
	w.terminated = make(chan any)

	w.wg.Add(2)
	go w.runWorker(cmd)
	go w.translateLogs(stdout)

	return nil
}

func (w *ProxyWorker) terminate() {
	w.mu.Lock()
	defer w.mu.Unlock()

	w.cancel()
	w.wg.Wait()
}

func (w *ProxyWorker) runWorker(cmd *exec.Cmd) {
	defer w.wg.Done()
	defer func() {
		_ = os.RemoveAll(w.workdir)
		close(w.terminated)
	}()

	if err := cmd.Start(); err != nil {
		w.log.Error("exec error", "error", err)
		return
	}

	pid := cmd.Process.Pid
	w.log.Debug("worker started", "pid", pid)

	if err := cmd.Wait(); err != nil {
		if !errors.Is(err, context.Canceled) {
			w.log.Error("exec error", "error", err)
		}
	}

	w.log.Debug("worker stopped", "pid", pid)
}

func (w *ProxyWorker) translateLogs(in io.Reader) {
	defer w.wg.Done()

	scanner := bufio.NewScanner(in)
	for {
		if !scanner.Scan() {
			return
		}

		record, err := parseProxyWorkerLogEntry(scanner.Bytes())
		if err != nil {
			continue
		}

		_ = w.log.With("module", "proxy.worker").Handler().Handle(context.Background(), record)
	}
}

func parseProxyWorkerLogEntry(entry []byte) (slog.Record, error) {
	buf := bytes.NewBuffer(entry)

	parsingToken := false
	var prefixTokens []string
	var currentPrefixToken []rune
	// parse all prefixes
	for {
		r, _, err := buf.ReadRune()
		if err == io.EOF {
			break
		}
		if err != nil {
			return slog.Record{}, err
		}

		if r == '[' {
			parsingToken = true
			continue
		}
		if r == ']' {
			parsingToken = false
			prefixTokens = append(prefixTokens, string(currentPrefixToken))
			currentPrefixToken = nil
			continue
		}
		if parsingToken {
			currentPrefixToken = append(currentPrefixToken, r)
			continue
		}
		if r == ' ' {
			continue
		}

		_ = buf.UnreadRune()
		break
	}

	msg, _ := buf.ReadString('\n')
	prefixTokens = lo.Without(prefixTokens, "console")

	// parse the time entry
	logTime, err := time.ParseInLocation("2006-01-02 15:04:05.000", prefixTokens[0], time.Local)
	if err != nil {
		fmt.Println(err)
		return slog.Record{}, err
	}

	level := slog.LevelInfo
	if len(prefixTokens) > 1 {
		switch prefixTokens[1] {
		case "trace":
			level = slog.LevelDebug
		case "debug":
			level = slog.LevelDebug
		case "info":
			level = slog.LevelInfo
		case "warning":
			level = slog.LevelWarn
		case "error":
			level = slog.LevelError
		}
	}

	sourceLocation := ""
	if len(prefixTokens) > 2 {
		sourceLocation = prefixTokens[2]
	}

	record := slog.NewRecord(logTime, level, msg, 0)
	if sourceLocation != "" {
		record.AddAttrs(slog.Attr{Key: "location", Value: slog.StringValue(sourceLocation)})
	}

	return record, nil
}
