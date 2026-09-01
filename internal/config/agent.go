// Package config loads the agent's configuration file.
//
// Flags and YAML only, as the legacy proxy had (§6.2). What the agent owns is provisioning-level
// — node name, filesystem authority, fabric attachments, server URLs, port range — and it changes
// when the host is built rather than when a flow is routed. What it no longer owns is
// subscriptions: that state lives in the API now, which is what collapses the agent's restart rate
// down to upgrades and removed the need for the companion reloader binary (§6.1).
//
// **It also no longer owns domain *names*.** *This supersedes a `domains:` block kept
// byte-compatible with the legacy proxy's, on the reasoning that it costs nothing to keep because
// it changes when a host is built.* Both halves of that turned out to be wrong (§16): it bought
// §10.6 an exception and a rule to police it, a rejection code checked at both ends and a
// `Configured` flag that outlived its purpose — and naming a domain is the one thing on this list
// an operator does while *routing* rather than while building, where doing it here cost an agent
// restart and an agent restart re-establishes every flow on the node (§6.1). Domains are
// discovered and named through the API instead (§6, §10.7).
//
// # Why a file at all, when everything else is a flag
//
// Fabric attachments are a list of records (§10.1), which does not fit on a command line.
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/jonasohland/mxl-replicator/internal/agent/probe"
	"github.com/jonasohland/mxl-replicator/internal/api"
)

// Agent is one agent configuration file.
//
// Every field is optional: the file supplies what the flags did not, and a deployment that is
// happy on the command line needs no file at all.
type Agent struct {
	// Node is the fleet-wide unique node name (§7.1).
	Node string `yaml:"node"`

	// Server is one or more control-plane URLs.
	Server []string `yaml:"server"`

	// Areas are the directories this node has designated as somewhere MXL domains live (§10.6):
	//
	//	areas:
	//	  - {name: media, path: /dev/shm/mxl,            read: true}
	//	  - {name: fast,  path: /dev/shm/mxl/replicated, read: true, write: true}
	//	  - {name: bulk,  path: /mnt/nvme/mxl,           read: true, write: true}
	//
	// *This supersedes `search_paths:` and `output_roots:` as separate blocks.* They were already
	// counterparts and already had to be read as a pair; one noun with two independent grants is
	// what that was asking for. The arrangement an operator actually reaches for — one MXL area
	// per host, with a subtree replication may write into — stops being an exception to an overlap
	// rule and becomes two ordinary areas.
	//
	// A list rather than a map, matching §10.6's own spelling and the `fabrics:` block next to it.
	// No default: a node with no area at all offers no sources and accepts no destinations, which
	// is the right posture for the one piece of configuration that grants this project any
	// authority over a host's filesystem.
	//
	// Several are supported because "this domain on tmpfs, that one on NVMe" is a real
	// requirement, and because an area is the natural place to hang a future capacity budget —
	// capacity being a property of a mount rather than of a domain.
	Areas []api.Area `yaml:"areas"`

	// Fabrics is what this node can be reached on (§10.1). Each entry is a (provider, fabric)
	// pair plus its selectors; the join against the worker's probe is [probe.Join].
	Fabrics []probe.Attachment `yaml:"fabrics"`
}

// LoadAgent reads and merges configuration files, in order.
//
// Merge rule, chosen to be predictable rather than clever: **a later file replaces a list it
// declares and leaves alone one it does not**. An attachment list is one description of a node's
// connectivity and half-overriding it is never what anyone means.
func LoadAgent(paths ...string) (*Agent, error) {
	merged := &Agent{}
	for _, path := range paths {
		loaded, err := loadAgentFile(path)
		if err != nil {
			return nil, err
		}
		merged.merge(loaded)
	}
	return merged, nil
}

func loadAgentFile(path string) (*Agent, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("config: read %s: %w", path, err)
	}

	var loaded Agent
	decoder := yaml.NewDecoder(strings.NewReader(string(raw)))
	// Unknown keys are an error, with no exceptions: a mistyped `fabrics:` that silently did
	// nothing would present as a node with no connectivity, which reads as missing hardware rather
	// than a typo. *The one tolerated case used to be inside a domain mapping, where the keys were
	// the legacy file's own; that block is gone (§16).*
	decoder.KnownFields(true)
	if err := decoder.Decode(&loaded); err != nil && err.Error() != "EOF" {
		return nil, fmt.Errorf("config: parse %s: %w", path, err)
	}

	return &loaded, nil
}

func (a *Agent) merge(other *Agent) {
	if other.Node != "" {
		a.Node = other.Node
	}
	if len(other.Server) > 0 {
		a.Server = slices.Clone(other.Server)
	}
	if len(other.Fabrics) > 0 {
		a.Fabrics = slices.Clone(other.Fabrics)
	}
	if len(other.Areas) > 0 {
		a.Areas = slices.Clone(other.Areas)
	}
}

// Validate checks everything that can be checked without touching the host.
func (a *Agent) Validate() error {
	var errs []error

	// Per-entry only. Whether two areas share a path is a property of the *merged* configuration —
	// file plus flags — so it is checked once there rather than twice with one of them seeing half
	// the picture (§10.6).
	for i, area := range a.Areas {
		if err := api.ValidDomainName(area.Name); err != nil {
			errs = append(errs, fmt.Errorf("areas[%d]: name %q: %w", i, area.Name, err))
		}
		if !filepath.IsAbs(area.Path) {
			errs = append(errs, fmt.Errorf("area %q: path %q must be absolute", area.Name, area.Path))
		}
		if !area.Read && !area.Write {
			// A line that does nothing. Refused rather than ignored, because an operator who wrote
			// it believes the node has an area there (§10.6).
			errs = append(errs, fmt.Errorf("area %q grants neither read nor write", area.Name))
		}
	}
	for i, attachment := range a.Fabrics {
		if err := attachment.Validate(); err != nil {
			errs = append(errs, fmt.Errorf("fabrics[%d]: %w", i, err))
		}
	}

	return errors.Join(errs...)
}

// ParseArea reads an area from a compact `name=path:grants` flag value (§10.6):
//
//	media=/dev/shm/mxl:r
//	fast=/dev/shm/mxl/replicated:rw
//	bulk=/mnt/nvme/mxl:w
//
// The flag exists for the common case; anything more wants the `areas:` YAML block, which is the
// precedent `fabrics:` already sets.
//
// **The grants are not optional and there is no default.** Both default false in the model
// (§10.6), so a flag that guessed would be granting this project authority over a host's
// filesystem on the strength of an omission — and an area granting neither is refused anyway, so
// omitting them could only ever mean an error with a worse message.
//
// The path is taken as everything between the first `=` and the last `:`, so a directory
// containing `=` works and one containing `:` is the operator's problem, which is the right way
// round: the grant suffix is this project's syntax and the path is the host's.
func ParseArea(value string) (api.Area, error) {
	name, rest, ok := strings.Cut(value, "=")
	if !ok {
		return api.Area{}, fmt.Errorf("area %q: expected name=path:grants, e.g. media=/dev/shm/mxl:r", value)
	}

	path, grants, ok := cutLast(rest, ":")
	if !ok {
		return api.Area{}, fmt.Errorf("area %q: names no grants; append :r, :w or :rw", value)
	}

	area := api.Area{Name: strings.TrimSpace(name), Path: strings.TrimSpace(path)}
	for _, grant := range grants {
		switch grant {
		case 'r':
			area.Read = true
		case 'w':
			area.Write = true
		default:
			return api.Area{}, fmt.Errorf("area %q: unknown grant %q, want some of r and w", value, string(grant))
		}
	}
	if !area.Read && !area.Write {
		return api.Area{}, fmt.Errorf("area %q: names no grants; append :r, :w or :rw", value)
	}
	return area, nil
}

// cutLast is [strings.Cut] on the *last* occurrence, which is what lets a path hold the separator
// this syntax also uses.
func cutLast(s, sep string) (before, after string, found bool) {
	i := strings.LastIndex(s, sep)
	if i < 0 {
		return s, "", false
	}
	return s[:i], s[i+len(sep):], true
}

// ParseFabric reads an attachment from a compact `key=value,key=value` flag value:
//
//	provider=tcp,fabric=dc1-data,interface=eth1
//	provider=tcp,fabric=dc1-data,network=10.1.0.0/16
//	provider=verbs,fabric=ib-a,device=mlx5_0,ip_version=4
//	provider=efa,fabric=vpc1-subnet-a
//	provider=shm
//
// The flag exists so that a single-host or development deployment needs no file at all — which
// is the case `mxl-replicator run` with no arguments is for (§2.2). Anything with more than a
// couple of attachments wants the YAML block.
func ParseFabric(value string) (probe.Attachment, error) {
	var attachment probe.Attachment

	for field := range strings.SplitSeq(value, ",") {
		field = strings.TrimSpace(field)
		if field == "" {
			continue
		}

		key, val, ok := strings.Cut(field, "=")
		if !ok {
			return probe.Attachment{}, fmt.Errorf("fabric %q: %q is not key=value", value, field)
		}
		key, val = strings.TrimSpace(key), strings.TrimSpace(val)

		switch key {
		case "provider":
			// Not checked here: Attachment.Validate rejects a provider this project cannot
			// negotiate, and it names the ones it can.
			attachment.Provider = api.Provider(val)
		case "fabric":
			attachment.Fabric = val
		case "address":
			attachment.Address = val
		case "interface":
			attachment.Interface = val
		case "device":
			attachment.Device = val
		case "network":
			attachment.Network = val
		case "ip_version":
			version, err := strconv.Atoi(val)
			if err != nil {
				return probe.Attachment{}, fmt.Errorf("fabric %q: ip_version %q is not a number", value, val)
			}
			attachment.IPVersion = version
		default:
			return probe.Attachment{}, fmt.Errorf("fabric %q: unknown key %q (want provider, fabric, address, interface, device, network or ip_version)", value, key)
		}
	}

	if err := attachment.Validate(); err != nil {
		return probe.Attachment{}, fmt.Errorf("fabric %q: %w", value, err)
	}
	return attachment, nil
}
