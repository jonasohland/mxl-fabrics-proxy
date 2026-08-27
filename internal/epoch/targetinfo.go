// Package epoch decodes a target worker's target_info blob and derives the epoch that
// identifies one target-worker incarnation (§5.2).
//
// # Why an epoch exists at all
//
// target_info is a serialised set of RDMA memory-registration keys for one specific process's
// specific memory mappings. It is invalidated by *any* restart of the target worker: a stale
// blob does not reconnect, it points at rkeys that no longer exist. And target workers restart
// routinely — restart is the worker's only recovery mechanism.
//
// So the pairing is inherently stateful, and with a central control plane the server is on the
// critical path of every re-establishment. The epoch is what makes that tractable without
// keepalives, change-detection RPCs or teardown negotiation: the destination agent reports an
// epoch alongside the blob, an initiator assignment is keyed by it, and the source agent's
// entire reconcile rule is one equality test — if the epoch I am running differs from the
// epoch I am assigned, tear down and start a new worker with the new blob. Server restarts,
// agent restarts and network partitions all fall out of that with no extra code.
//
// # Coupling to mxl-fabrics
//
// The TargetInfo structure is not part of MXL's public API. Reaching into it is acceptable
// only because this project and mxl-fabrics have the same maintainer — which makes it a
// coupling that has to be recorded on *both* sides, or someone refactors TargetInfo and
// silently changes epoch semantics with no build failure anywhere.
//
// This side of the guard is [Decode]'s unknown-field reporting. It warns, and never fails: an
// unknown field is far more likely to be additive and harmless than epoch-relevant, and
// failing closed would take out replication on an unrelated mxl upgrade. The other side is a
// comment at the TargetInfo definition in mxl-fabrics naming this project as a consumer of the
// field set.
package epoch

import (
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"
)

// U64 is a uint64 that the library serialises as a decimal *string* — `"addr": "0"`,
// `"rkey": "17918262359965949928"` — because these values do not survive a JSON number in
// consumers that decode through a float64.
//
// Decoding accepts a bare number too, so a future library that stops quoting them does not
// break the epoch.
type U64 uint64

func (u *U64) UnmarshalJSON(data []byte) error {
	text := string(data)
	if quoted, err := strconv.Unquote(text); err == nil {
		text = quoted
	}

	value, err := strconv.ParseUint(strings.TrimSpace(text), 10, 64)
	if err != nil {
		return fmt.Errorf("expected a uint64 (as a string or a number), got %s", data)
	}

	*u = U64(value)
	return nil
}

// MarshalJSON emits the quoted form the library uses.
func (u U64) MarshalJSON() ([]byte, error) {
	return []byte(strconv.Quote(strconv.FormatUint(uint64(u), 10))), nil
}

// Region is one registered memory region: where it is, how big it is, and the remote key the
// NIC needs to write into it.
type Region struct {
	Addr U64 `json:"addr"`
	Len  U64 `json:"len"`
	RKey U64 `json:"rkey"`
}

// BounceBufferInfo describes the bounce-buffer ring, present for continuous (audio) flows only.
type BounceBufferInfo struct {
	EntryCount U64 `json:"entryCount"`
	EntrySize  U64 `json:"entrySize"`
}

// TargetInfo is the decoded blob (WRS §4).
//
// Decoded here only to compute and check the epoch. Everywhere else in this project the blob
// is opaque: it is transported as the string the worker wrote and handed to the peer worker
// unmodified.
type TargetInfo struct {
	// ID is an endpoint identifier of unspecified derivation, and is the only field the worker
	// itself looks at — it rejects a blob without one.
	ID string `json:"id"`

	// AddressFormat and Provider are deliberately **not** hashed: a change in either only ever
	// arrives with a new session, because the server assigns the provider (§5.2, §10).
	AddressFormat int    `json:"addressFormat"`
	Provider      string `json:"provider"`

	// FabricAddress embeds ip:port for tcp and verbs, base64-encoded.
	FabricAddress string `json:"fabricAddress"`

	Regions []Region `json:"regions"`

	// BounceBuffer is absent for discrete (video, data) flows.
	BounceBuffer *BounceBufferInfo `json:"bounceBufferInfo,omitempty"`
}

var (
	targetInfoKeys  = []string{"id", "addressFormat", "provider", "fabricAddress", "regions", "bounceBufferInfo"}
	regionKeys      = []string{"addr", "len", "rkey"}
	bounceBufferKey = []string{"entryCount", "entrySize"}
)

// Decode parses a target_info blob.
//
// The second return value lists the paths of fields the library emitted that this package does
// not know about — `newField`, `regions[0].newField`, `bounceBufferInfo.newField`. It is a
// warning for the caller to log, never an error: see the package doc. When a field is added
// upstream, this project says so instead of silently omitting it from the hash.
//
// Decode does fail on a blob that is not usable: invalid JSON, a known field of the wrong
// type, or a missing `id`. The last one mirrors the worker's own check (WRS §4), so a
// truncated or wrong blob fails here with a clear message rather than in a worker restart loop
// where it looks like a fabric problem.
func Decode(blob string) (*TargetInfo, []string, error) {
	// A pre-M0 worker wrote the blob with a trailing NUL, because the library's length includes
	// the terminator and most JSON parsers reject anything after the top-level value. Current
	// workers strip it at the source; tolerating it here costs one call and means a mixed-version
	// deployment is not a mystery.
	trimmed := strings.TrimRight(blob, "\x00 \t\r\n")
	if trimmed == "" {
		return nil, nil, fmt.Errorf("target info is empty")
	}

	var raw map[string]json.RawMessage
	if err := json.Unmarshal([]byte(trimmed), &raw); err != nil {
		return nil, nil, fmt.Errorf("target info: %w", err)
	}

	unknown := unknownKeys("", raw, targetInfoKeys)

	if encoded, ok := raw["regions"]; ok {
		var regions []map[string]json.RawMessage
		if err := json.Unmarshal(encoded, &regions); err == nil {
			for i, region := range regions {
				unknown = append(unknown, unknownKeys(fmt.Sprintf("regions[%d]", i), region, regionKeys)...)
			}
		}
	}

	if encoded, ok := raw["bounceBufferInfo"]; ok {
		var bounce map[string]json.RawMessage
		if err := json.Unmarshal(encoded, &bounce); err == nil {
			unknown = append(unknown, unknownKeys("bounceBufferInfo", bounce, bounceBufferKey)...)
		}
	}

	var info TargetInfo
	if err := json.Unmarshal([]byte(trimmed), &info); err != nil {
		return nil, unknown, fmt.Errorf("target info: %w", err)
	}
	if info.ID == "" {
		return nil, unknown, fmt.Errorf("target info: missing id")
	}

	slices.Sort(unknown)
	return &info, unknown, nil
}

func unknownKeys(prefix string, object map[string]json.RawMessage, known []string) []string {
	var unknown []string
	for key := range object {
		if slices.Contains(known, key) {
			continue
		}
		if prefix == "" {
			unknown = append(unknown, key)
		} else {
			unknown = append(unknown, prefix+"."+key)
		}
	}
	return unknown
}
