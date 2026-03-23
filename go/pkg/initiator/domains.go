package initiator

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/google/uuid"
	"github.com/jonasohland/mxl-fabrics-proxy/go/pkg/common"
	"github.com/jonasohland/mxl-fabrics-proxy/go/pkg/server"
)

type Domain struct {
	dURL     *url.URL
	name     string
	node     string
	provider string
}

type Domains struct {
	mu      sync.Mutex
	domains map[string]*Domain
}

func NewDomains() *Domains {
	return &Domains{sync.Mutex{}, make(map[string]*Domain)}
}

func (d *Domains) Mux(mux *http.ServeMux) {
	mux.Handle("/v1/flows/", d)
}

func (d *Domains) Add(domainURL, name, mappedNode, provider string) (string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	durl, err := url.Parse(domainURL)
	if err != nil {
		return "", err
	}

	if durl.Scheme != "mxl" {
		return "", fmt.Errorf("invalid protocol scheme: %s", durl.Scheme)
	}

	id, err := common.NewCookie()
	if err != nil {
		return "", err
	}

	if name == "" {
		name = durl.Path
	}

	slog.Info("adding domain", "path", durl.Path)
	d.domains[id] = &Domain{dURL: durl, name: name, node: mappedNode, provider: provider}

	return id, nil
}

func (d *Domains) Remove(id string) {
	d.mu.Lock()
	defer d.mu.Unlock()

	mapping, ok := d.domains[id]
	if ok {
		slog.Info("remove domain", "path", mapping.dURL.Path)
		delete(d.domains, id)
	}
}

func (d *Domains) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	domainPath := strings.TrimPrefix(r.URL.Path, "/v1/flows")
	domainInfo, err := d.readDomain(domainPath, r.URL.Query()["id"])
	if err != nil {
		server.Error(w, err)
		return
	}

	_ = json.NewEncoder(w).Encode(domainInfo)
}

func (d *Domains) Find(domain string, id string) (*common.DomainInfo, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	return d.readDomain(domain, []string{id})
}

func (d *Domains) findDomain(nameOrPath string) (*Domain, error) {
	for _, domain := range d.domains {
		if domain.dURL.Path == nameOrPath || domain.name == strings.TrimPrefix(nameOrPath, "/") {
			return domain, nil
		}
	}

	return nil, server.ErrNotFound
}

func (d *Domains) readDomain(nameOrPath string, ids []string) (*common.DomainInfo, error) {
	domain, err := d.findDomain(nameOrPath)
	if err != nil {
		return nil, err
	}

	if len(ids) > 0 {
		return domain.readFlows(ids)
	} else {
		return domain.read()
	}
}

func (d *Domain) readFlows(ids []string) (*common.DomainInfo, error) {
	out := &common.DomainInfo{
		Path:  d.dURL.Path,
		Name:  d.name,
		Node:  d.node,
		Flows: make(map[string]common.FlowInfo, len(ids)),
	}
	for _, id := range ids {
		flowInfo, err := d.readFlow(id)
		if err != nil {
			return nil, err
		}

		out.Flows[id] = flowInfo
	}

	return out, nil
}

func (d *Domain) read() (*common.DomainInfo, error) {
	entries, err := os.ReadDir(d.dURL.Path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, server.ErrNotFound
		}

		return nil, err
	}

	flows := make(map[string]common.FlowInfo, len(entries))

	for _, entry := range entries {
		flowID := strings.TrimSuffix(entry.Name(), ".mxl-flow")
		_, err := uuid.Parse(flowID)
		if err != nil {
			continue
		}

		info, err := d.readFlow(flowID)
		if err != nil {
			continue
		}

		flows[flowID] = info
	}

	return &common.DomainInfo{
		Path:  d.dURL.Path,
		Node:  d.node,
		Name:  d.name,
		Flows: flows,
	}, nil
}

func (d *Domain) readFlow(id string) (common.FlowInfo, error) {
	var out common.FlowInfo

	fd, err := os.OpenFile(filepath.Join(d.dURL.Path, id+".mxl-flow", "flow_def.json"), os.O_RDONLY, 0000)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return out, server.ErrNotFound
		}

		return out, err
	}
	defer func() { _ = fd.Close() }()

	if err := json.NewDecoder(fd).Decode(&out.FlowDefinition); err != nil {
		return out, err
	}

	return out, nil
}
