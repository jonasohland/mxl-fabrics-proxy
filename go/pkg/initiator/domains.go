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
	dURL *url.URL
}

type Domains struct {
	mu      sync.Mutex
	domains []*Domain
}

func NewDomains() *Domains {
	return &Domains{sync.Mutex{}, make([]*Domain, 0)}
}

func (d *Domains) Mux(mux *http.ServeMux) {
	mux.Handle("/v1/flows/", d)
}

func (d *Domains) Add(domainURL string) error {
	durl, err := url.Parse(domainURL)
	if err != nil {
		return err
	}

	if durl.Scheme != "mxl" {
		return fmt.Errorf("invalid protocol scheme: %s", durl.Scheme)
	}

	slog.Info("adding domain", "path", durl.Path)
	d.domains = append(d.domains, &Domain{dURL: durl})

	return nil
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

func (d *Domains) findDomain(path string) (*Domain, error) {
	for _, domain := range d.domains {
		if domain.dURL.Path == path {
			return domain, nil
		}
	}

	return nil, server.ErrNotFound
}

func (d *Domains) readDomain(path string, ids []string) (*common.DomainInfo, error) {
	domain, err := d.findDomain(path)
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
	defer fd.Close()

	if err := json.NewDecoder(fd).Decode(&out.FlowDefinition); err != nil {
		return out, err
	}

	return out, nil
}
