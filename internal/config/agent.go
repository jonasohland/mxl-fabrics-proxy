// Package config loads the agent's configuration file.
//
// Flags and YAML only, as the legacy proxy had (§6.2). What the agent owns is provisioning-level
// — node name, domain mappings, fabric attachments, server URLs, port range — and it changes when
// the host is built rather than when a flow is routed. What it no longer owns is subscriptions:
// that state lives in the API now, which is what collapses the agent's restart rate down to
// upgrades and removed the need for the companion reloader binary (§6.1).
//
// # Why a file at all, when everything else is a flag
//
// Two things do not fit on a command line. Fabric attachments are a list of records (§10.1), and
// the domain block has to stay byte-compatible with the legacy proxy's so that an existing
// deployment's mapping config carries over unchanged (§16) — which means accepting the shape that
// config was written in, not only the shape that is convenient now.
package config

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"slices"
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

	// Domains is the name→path mapping block, kept legacy-compatible (§16). See [Domains].
	Domains Domains `yaml:"domains"`

	// SearchPaths are recursively scanned for unconfigured domains. A domain found this way is a
	// replication source; nothing discovered is ever written to (§7.2, §10.6).
	SearchPaths []string `yaml:"search_paths"`

	// OutputRoots are the directories replication may create domains under (§10.6):
	//
	//	output_roots:
	//	  - name: fast
	//	    path: /dev/shm/mxl
	//	  - name: bulk
	//	    path: /mnt/nvme/mxl
	//
	// A list rather than a map, matching §10.6's own spelling and the `fabrics:` block next to
	// it. No default and no legacy equivalent: this is the opt-in that makes a node a replication
	// destination, and a node with none configured accepts none.
	//
	// Several are supported because "this domain on tmpfs, that one on NVMe" is a real
	// requirement, and because a root is the natural place to hang a future capacity budget —
	// capacity being a property of a mount rather than of a domain.
	OutputRoots []OutputRoot `yaml:"output_roots"`

	// Fabrics is what this node can be reached on (§10.1). Each entry is a (provider, fabric)
	// pair plus at most one selector; the join against the worker's probe is [probe.Join].
	Fabrics []probe.Attachment `yaml:"fabrics"`
}

// OutputRoot is one entry of the `output_roots:` block.
//
// Deliberately not reusing [Domains]' shape: a root is not a domain, and giving them one spelling
// would invite exactly the confusion §10.6 exists to remove — an input mapping is a directory the
// node *has*, a root is a place the control plane is permitted to create directories.
type OutputRoot struct {
	Name string `yaml:"name"`
	Path string `yaml:"path"`
}

// Domains is the domain mapping block.
//
// It decodes both the shape this project writes and the shape the legacy proxy's `config.yaml`
// used, because §16 promises the mapping config carries over:
//
//	domains:
//	  cameras: /dev/shm/mxl0                  # this project
//	  ingest:
//	    url: mxl:///dev/shm/mxl1              # legacy
//	  archive:
//	    path: /dev/shm/mxl2                   # explicit, equivalent to the scalar form
//
// The legacy block carried `node`, `provider` and `labels` alongside `url`. None of them survive
// the rewrite as domain-level settings — a provider is negotiated per session against declared
// attachments (§10), and labels belong to a request — and they are ignored rather than rejected
// so that a legacy file can be pointed at this agent unchanged while its subscriptions are
// imported separately (M8).
type Domains map[string]string

// UnmarshalYAML decodes either spelling of a domain mapping.
func (d *Domains) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind != yaml.MappingNode {
		return fmt.Errorf("domains: expected a mapping of name to path, got %s", kindName(node.Kind))
	}

	out := Domains{}
	for i := 0; i+1 < len(node.Content); i += 2 {
		key, value := node.Content[i], node.Content[i+1]

		var name string
		if err := key.Decode(&name); err != nil {
			return fmt.Errorf("domains: %w", err)
		}

		path, err := domainPath(name, value)
		if err != nil {
			return err
		}
		out[name] = path
	}

	*d = out
	return nil
}

func domainPath(name string, value *yaml.Node) (string, error) {
	switch value.Kind {
	case yaml.ScalarNode:
		var scalar string
		if err := value.Decode(&scalar); err != nil {
			return "", fmt.Errorf("domain %q: %w", name, err)
		}
		return resolveDomainValue(name, scalar)

	case yaml.MappingNode:
		// The legacy shape. Unknown keys are ignored on purpose — see the type comment.
		var mapping struct {
			URL  string `yaml:"url"`
			Path string `yaml:"path"`
		}
		if err := value.Decode(&mapping); err != nil {
			return "", fmt.Errorf("domain %q: %w", name, err)
		}
		switch {
		case mapping.Path != "" && mapping.URL != "":
			return "", fmt.Errorf("domain %q: set either url or path, not both", name)
		case mapping.Path != "":
			return resolveDomainValue(name, mapping.Path)
		case mapping.URL != "":
			return resolveDomainValue(name, mapping.URL)
		default:
			return "", fmt.Errorf("domain %q: neither url nor path is set", name)
		}

	default:
		return "", fmt.Errorf("domain %q: expected a path or a mapping, got %s", name, kindName(value.Kind))
	}
}

// resolveDomainValue accepts a plain path or an `mxl://` URL.
func resolveDomainValue(name, value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", fmt.Errorf("domain %q: empty path", name)
	}

	if !strings.Contains(value, "://") {
		return cleanAbs(name, value)
	}

	parsed, err := url.Parse(value)
	if err != nil {
		return "", fmt.Errorf("domain %q: %w", name, err)
	}
	if parsed.Scheme != "mxl" {
		return "", fmt.Errorf("domain %q: scheme %q is not mxl", name, parsed.Scheme)
	}
	if parsed.Host != "" {
		// The legacy config used `mxl://host/path` for *remote* domains, which this agent has no
		// business mapping: a domain block names directories on this host, and a remote flow is
		// addressed by (node, domain) through the API now.
		return "", fmt.Errorf("domain %q: %q names a host, but a domain mapping is local to this node", name, value)
	}
	return cleanAbs(name, parsed.Path)
}

func cleanAbs(name, path string) (string, error) {
	if !filepath.IsAbs(path) {
		// A relative path is interpreted against the agent's working directory, which is not
		// something the operator controls under a DaemonSet.
		return "", fmt.Errorf("domain %q: path %q must be absolute", name, path)
	}
	return filepath.Clean(path), nil
}

// LoadAgent reads and merges configuration files, in order.
//
// Merge rules, chosen to be predictable rather than clever: **maps merge per key and lists
// replace**. A later file adding a domain keeps the earlier ones; a later file with a `fabrics:`
// block replaces the whole list, because an attachment list is one description of a node's
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
	// Unknown keys are an error: a mistyped `fabrics:` that silently did nothing would present
	// as a node with no connectivity, which reads as missing hardware rather than a typo. The
	// one place unknown keys are tolerated is inside a domain mapping, where they are the legacy
	// file's own fields (see [Domains]).
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
	if len(other.SearchPaths) > 0 {
		a.SearchPaths = slices.Clone(other.SearchPaths)
	}
	if len(other.OutputRoots) > 0 {
		a.OutputRoots = slices.Clone(other.OutputRoots)
	}
	for name, path := range other.Domains {
		if a.Domains == nil {
			a.Domains = Domains{}
		}
		a.Domains[name] = path
	}
}

// Validate checks everything that can be checked without touching the host.
func (a *Agent) Validate() error {
	var errs []error

	for name, path := range a.Domains {
		// The same rule the inventory applies, so a name is refused where it is typed rather than
		// several layers down (§10.6).
		if err := api.ValidDomainName(name); err != nil {
			errs = append(errs, fmt.Errorf("domains: name %q: %w", name, err))
		}
		if !filepath.IsAbs(path) {
			errs = append(errs, fmt.Errorf("domain %q: path %q must be absolute", name, path))
		}
	}
	for _, path := range a.SearchPaths {
		if !filepath.IsAbs(path) {
			errs = append(errs, fmt.Errorf("search path %q must be absolute", path))
		}
	}
	// Per-entry only. Whether the roots overlap each other, a domain mapping or a search path is
	// a property of the *merged* configuration — file plus flags — so it is checked once there
	// rather than twice with one of them seeing half the picture (§10.6).
	for i, root := range a.OutputRoots {
		if err := api.ValidDomainName(root.Name); err != nil {
			errs = append(errs, fmt.Errorf("output_roots[%d]: name %q: %w", i, root.Name, err))
		}
		if !filepath.IsAbs(root.Path) {
			errs = append(errs, fmt.Errorf("output root %q: path %q must be absolute", root.Name, root.Path))
		}
	}
	for i, attachment := range a.Fabrics {
		if err := attachment.Validate(); err != nil {
			errs = append(errs, fmt.Errorf("fabrics[%d]: %w", i, err))
		}
	}

	return errors.Join(errs...)
}

// ParseFabric reads an attachment from a compact `key=value,key=value` flag value:
//
//	provider=tcp,fabric=dc1-data,interface=eth1
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
		default:
			return probe.Attachment{}, fmt.Errorf("fabric %q: unknown key %q (want provider, fabric, address, interface or device)", value, key)
		}
	}

	if err := attachment.Validate(); err != nil {
		return probe.Attachment{}, fmt.Errorf("fabric %q: %w", value, err)
	}
	return attachment, nil
}

func kindName(kind yaml.Kind) string {
	switch kind {
	case yaml.SequenceNode:
		return "a list"
	case yaml.MappingNode:
		return "a mapping"
	case yaml.ScalarNode:
		return "a scalar"
	default:
		return "something else"
	}
}
