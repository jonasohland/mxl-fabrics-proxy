package exec

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"testing"
	"time"
)

// The tests in this package run the launcher against a real process, because a launcher tested
// only against itself is exactly the gap this rewrite is closing. The process is this test
// binary, re-executed: TestMain notices the marker in its environment and behaves like
// mxl-fabrics-proxy-worker instead of running tests.
//
// The marker is passed through [Options.Env] rather than set on the test process, so nothing
// here depends on ambient environment — and it exercises that option at the same time.
const (
	helperEnv = "MXL_REPLICATOR_TEST_WORKER"

	// Behaviour knobs for the fake worker, all optional.
	helperExitEnv          = "MXL_REPLICATOR_TEST_EXIT"          // exit immediately with this code
	helperNoTargetInfoEnv  = "MXL_REPLICATOR_TEST_NO_INFO"       // never write target-info.json
	helperInfoDelayEnv     = "MXL_REPLICATOR_TEST_INFO_DELAY_MS" // wait before writing it
	helperIgnoreSigtermEnv = "MXL_REPLICATOR_TEST_IGNORE_TERM"   // survive SIGTERM
	helperPartialInfoEnv   = "MXL_REPLICATOR_TEST_PARTIAL_INFO"  // write a truncated blob first
	helperMetricsEnv       = "MXL_REPLICATOR_TEST_METRICS"       // serve this on the metrics socket
	helperProbeFailEnv     = "MXL_REPLICATOR_TEST_PROBE_FAIL"    // make --interfaces fail
)

// helperInterfaces is what --interfaces prints: a tcp entry with a netdev device_name, and an
// shm entry with the hostname as its address and no device_name at all (WRS §2).
const helperInterfaces = `[
  {"provider":"tcp","node":"10.135.0.123",
   "caps":{"flags":["REMOTE_WRITE","SEND_RECEIVE","BLOCKING_OPERATIONS"],
           "max_message_size":18446744073709551615},
   "attr":{"device_name":"eth1","ep_type":"FI_EP_MSG"}},
  {"provider":"shm","node":"edge-01",
   "caps":{"flags":["REMOTE_WRITE"],"max_message_size":1048576}}
]
`

// helperTargetInfo is what the fake worker writes, shaped like the library's (WRS §4).
const helperTargetInfo = `{"id":"1001","addressFormat":1,"fabricAddress":"10.0.2.4:24012",` +
	`"provider":"tcp","regions":[{"addr":"0","len":"1048576","rkey":"17918262359965949928"}]}`

func TestMain(m *testing.M) {
	if os.Getenv(helperEnv) != "" {
		os.Exit(runFakeWorker())
	}
	os.Exit(m.Run())
}

// runFakeWorker imitates enough of the worker for the launcher to be exercised end to end: it
// reads its config from argv[1], logs to stdout in spdlog's format, binds the metrics socket,
// writes target-info.json when it is a target, and exits on SIGTERM.
func runFakeWorker() int {
	if len(os.Args) < 2 {
		workerLog("error", "fatal: no config file")
		return 1
	}

	// The two probe modes (WRS §2). Note which stream each uses: -v is diagnostics and goes to
	// stderr, --interfaces is data and goes to stdout, and getting that backwards is exactly
	// the kind of thing only a real invocation catches.
	switch os.Args[1] {
	case "-v":
		fmt.Fprint(os.Stderr, "proxy     0.0.1\nmxl       1.1.0-rc1\nlibfabric 2.6\n")
		return 0
	case "--interfaces":
		if os.Getenv(helperProbeFailEnv) != "" {
			fmt.Fprintln(os.Stderr, "fatal: no fabric interfaces")
			return 1
		}
		fmt.Fprintln(os.Stderr, "libfabric:1234::core: registering provider: tcp")
		fmt.Fprint(os.Stdout, helperInterfaces)
		return 0
	}

	raw, err := os.ReadFile(os.Args[1])
	if err != nil {
		workerLog("error", "fatal: "+err.Error())
		return 1
	}
	var cfg config
	if err := json.Unmarshal(raw, &cfg); err != nil {
		workerLog("error", "fatal: "+err.Error())
		return 1
	}

	if code := os.Getenv(helperExitEnv); code != "" {
		workerLog("error", "fatal: asked to die")
		n, _ := strconv.Atoi(code)
		return n
	}

	// The signal disposition goes in before anything else, as it does in the real worker
	// (src/main.cpp installs its handlers straight after argument parsing). "connected" below
	// is then a promise a test can wait on: past that line, SIGTERM is handled.
	term := make(chan os.Signal, 1)
	if os.Getenv(helperIgnoreSigtermEnv) != "" {
		signal.Ignore(syscall.SIGTERM)
	} else {
		signal.Notify(term, syscall.SIGTERM, syscall.SIGINT)
	}

	// Echoed so a test can assert the agent's log level actually reached the process (§12);
	// the legacy supervisor never plumbed it through at all.
	workerLog("info", "MXL_LOG_LEVEL="+os.Getenv("MXL_LOG_LEVEL"))

	if listener, err := net.Listen("unix", cfg.MetricsSocket); err == nil {
		defer func() { _ = listener.Close() }()
		go serveMetrics(listener, os.Getenv(helperMetricsEnv))
	} else {
		workerLog("error", "fatal: "+err.Error())
		return 1
	}

	if cfg.Target && os.Getenv(helperNoTargetInfoEnv) == "" {
		go writeTargetInfo(cfg.TargetInfo)
	}

	workerLog("info", "connected")

	<-term
	workerLog("info", "interrupted, exiting")
	return 0
}

func writeTargetInfo(path string) {
	if ms := os.Getenv(helperInfoDelayEnv); ms != "" {
		n, _ := strconv.Atoi(ms)
		time.Sleep(time.Duration(n) * time.Millisecond)
	}
	// A truncated first write, so the partial-read guard in readTargetInfo is exercised the
	// way the worker's own non-atomic ofstream can produce it (WRS §5.1).
	if os.Getenv(helperPartialInfoEnv) != "" {
		_ = os.WriteFile(path, []byte(helperTargetInfo[:20]), 0o600)
		time.Sleep(20 * time.Millisecond)
	}
	_ = os.WriteFile(path, []byte(helperTargetInfo), 0o600)
}

func serveMetrics(listener net.Listener, body string) {
	if body == "" {
		body = "mxl_grains_total 300\nmxl_source_latency_ns[0.5] 498000\n"
	}
	for {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		_, _ = conn.Write([]byte(body))
		_ = conn.Close()
	}
}

// workerLog writes one line in spdlog's default pattern, which is what the agent parses back
// out (WRS §7).
func workerLog(level, message string) {
	fmt.Printf("[%s] [console] [%s] %s\n", time.Now().Format(timeLayoutForHelper), level, message)
}

const timeLayoutForHelper = "2006-01-02 15:04:05.000"
