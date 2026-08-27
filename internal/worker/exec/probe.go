package exec

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	osexec "os/exec"
	"strings"

	"github.com/jonasohland/mxl-replicator/internal/api"
)

// The probes live here rather than in the agent for the same reason [Launcher] does: this is
// the only package that runs the worker binary (invariant 11). What the agent owns is the
// *join* — reconciling what the library reports against what the operator configured (§10.5,
// M5b) — and that is a pure function over [Interface] values.

// ProbeVersions runs the worker's `-v` probe (WRS §2).
//
// It doubles as the load probe: it proves the binary exists and that its shared libraries
// resolve, before anything is assigned to this node. The legacy supervisor already did exactly
// this at startup and then threw the versions away.
//
// The versions are not decoration. `target_info` is produced by one node's mxl-fabrics and
// consumed by another's, so a node pair straddling an mxl version boundary is a compatibility
// concern *neither agent can detect alone* — only the server, which sees both sides (§10.2).
//
// Only the three fields the worker reports are filled in; the caller adds its own.
func ProbeVersions(ctx context.Context, binary string) (api.Versions, error) {
	if binary == "" {
		binary = DefaultBinary
	}

	// -v prints to stderr. Only --interfaces uses stdout, because only it emits data (WRS §2).
	var out, errOut bytes.Buffer
	cmd := osexec.CommandContext(ctx, binary, "-v")
	cmd.Stdout = &out
	cmd.Stderr = &errOut
	if err := cmd.Run(); err != nil {
		return api.Versions{}, fmt.Errorf("worker: %s -v: %w%s", binary, err, diagnostics(errOut.String()))
	}

	versions := api.Versions{}
	found := false
	for line := range strings.Lines(errOut.String()) {
		name, value, ok := strings.Cut(strings.TrimSpace(line), " ")
		if !ok {
			continue
		}
		value = strings.TrimSpace(value)
		switch name {
		case "proxy":
			versions.Proxy, found = value, true
		case "mxl":
			versions.MXL, found = value, true
		case "libfabric":
			versions.Libfabric, found = value, true
		}
	}
	if !found {
		return api.Versions{}, fmt.Errorf("worker: %s -v reported no versions%s", binary, diagnostics(errOut.String()))
	}
	return versions, nil
}

// Interface is one entry from the worker's `--interfaces` probe (WRS §2): one
// (interface, address, provider) combination that libfabric actually offers on this host.
//
// The agent cannot enumerate these itself — it is Go and does not link the library — and
// guessing from /dev/infiniband and interface names is a heuristic whose failure mode is a
// confusing restart loop (§10.5). This is ground truth from the library that will run the
// transfer.
type Interface struct {
	Provider api.Provider `json:"provider"`

	// Address is the probe's `node`, and it is what goes in the worker config's `node` key.
	// Named Address here for the same reason [api.FabricAttachment.Address] is: "node" means a
	// host everywhere else in this project.
	//
	// Provider-dependent — an IP for tcp and verbs, a link-local device address for efa, and
	// the **hostname** for shm.
	Address string `json:"node"`

	Caps InterfaceCaps `json:"caps"`

	// Attr is the library's best-effort attribute blob, passed through verbatim and absent
	// when it reports none. Contents vary by platform and hardware, so every key is optional.
	// [Interface.Device] reads the only one this project uses.
	Attr json.RawMessage `json:"attr,omitempty"`
}

// InterfaceCaps is what the library reports an interface can do.
type InterfaceCaps struct {
	Flags []api.CapFlag `json:"flags"`

	// MaxMessageSize is a genuine uint64 and providers do report UINT64_MAX. It must never
	// pass through a float64 — decoding into `any` loses it (WRS §2).
	MaxMessageSize uint64 `json:"max_message_size"`
}

// Device is the probe's attr.device_name, or "".
//
// This is as close as the API comes to naming a physical interface, and it is **not** a netdev
// name in general: it is one for tcp (`eth1`, `wlan0`), but the libfabric device name for verbs
// and efa (`mlx5_0`, `rdmap0s6-rdm`), and shm reports none at all. That is why a configured
// `ib0` cannot simply be looked up here, and why the join in §10.5 has four selectors rather
// than a name match.
func (i Interface) Device() string {
	if len(i.Attr) == 0 {
		return ""
	}
	var attr struct {
		DeviceName string `json:"device_name"`
	}
	if err := json.Unmarshal(i.Attr, &attr); err != nil {
		return ""
	}
	return attr.DeviceName
}

// ProbeInterfaces runs the worker's `--interfaces` probe (WRS §2).
//
// Entries are returned exactly as reported, including providers this project does not
// negotiate: filtering is part of the join against the operator's configured attachments, and
// an entry dropped here would be one the agent could not name in the "no match, here is what
// this node does have" message that makes a typo distinguishable from missing hardware
// (§10.5).
//
// Note there is deliberately no service in the result. The library reports one, but it is
// empty for every provider but shm and the shm value is an artefact of whichever process ran
// the probe — the agent allocates a service for every provider from its own range instead
// (§7.4, WRS §2, §9).
func ProbeInterfaces(ctx context.Context, binary string) ([]Interface, error) {
	if binary == "" {
		binary = DefaultBinary
	}

	// The two streams must be captured separately: the probe puts JSON on stdout and
	// libfabric's diagnostics on stderr, and the worker redirects its own stdout away for the
	// duration precisely so that the data stream stays parseable (WRS §2).
	var out, errOut bytes.Buffer
	cmd := osexec.CommandContext(ctx, binary, "--interfaces")
	cmd.Stdout = &out
	cmd.Stderr = &errOut
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("worker: %s --interfaces: %w%s", binary, err, diagnostics(errOut.String()))
	}

	var interfaces []Interface
	if err := json.Unmarshal(out.Bytes(), &interfaces); err != nil {
		return nil, fmt.Errorf("worker: %s --interfaces: parse output: %w%s", binary, err, diagnostics(errOut.String()))
	}
	return interfaces, nil
}

// diagnostics appends the worker's stderr to an error message, where its `fatal:` line is.
func diagnostics(stderr string) string {
	stderr = strings.TrimSpace(stderr)
	if stderr == "" {
		return ""
	}
	return ": " + strings.ReplaceAll(stderr, "\n", "; ")
}
