package config

import (
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/jonasohland/mxl-fabrics-proxy/go/pkg/initiator"
	"github.com/jonasohland/mxl-fabrics-proxy/go/pkg/server"
	"github.com/jonasohland/mxl-fabrics-proxy/go/pkg/target"
	"github.com/samber/lo"
)

type Runner struct {
	mu sync.Mutex

	cfg   *Config
	state state

	paths []string

	domains *initiator.Domains
	subs    *initiator.Subscriptions
	targets *target.Targets
}

// ServeHTTP implements [http.Handler].
func (r *Runner) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	if err := r.Reload(); err != nil {
		server.Error(w, fmt.Errorf("config file not reloaded: %s", err))
		return
	}

	server.Response(w, &server.Message{Message: "configuration reloaded", Code: http.StatusOK})
}

func NewRunner(cfg *Config, paths []string, domains *initiator.Domains, subs *initiator.Subscriptions, targets *target.Targets) (*Runner, error) {
	r := &Runner{
		mu: sync.Mutex{},

		cfg: cfg,
		state: state{
			subs:    map[string]*Subscription{},
			domains: map[string]*DomainMapping{},
		},

		paths: paths,

		domains: domains,
		subs:    subs,
		targets: targets,
	}

	if err := r.Reload(); err != nil {
		return nil, err
	}

	return r, nil
}

func (r *Runner) Mux(mux *http.ServeMux) {
	mux.Handle("POST /v1/reload", r)
}

func (r *Runner) Reload() error {
	r.mu.Lock()
	defer r.mu.Unlock()

	next, err := r.loadFromDisk()
	if err != nil {
		return err
	}

	if err := next.Normalize(); err != nil {
		return err
	}

	return r.reloadWith(next)
}

func (r *Runner) loadFromDisk() (*Config, error) {
	next := &Config{}

	for _, path := range r.paths {
		if strings.HasSuffix(path, ".yaml") || strings.HasSuffix(path, ".yml") {
			cfg, err := Load(path)
			if err != nil {
				return nil, err
			}

			next.merge(cfg)
			continue
		}

		entries, err := os.ReadDir(path)
		if err != nil {
			return nil, err
		}

		for _, entry := range entries {
			if strings.HasSuffix(entry.Name(), ".yaml") || strings.HasSuffix(entry.Name(), ".yml") {
				cfg, err := Load(filepath.Join(path, entry.Name()))
				if err != nil {
					return nil, err
				}

				next.merge(cfg)
			}
		}
	}

	return next, nil
}

func (r *Runner) reloadWith(next *Config) error {
	uniqueSubscriptions := make(map[string][]Subscription)
	for localDomain, subscriptions := range next.Subscriptions {
		seen := make([]Subscription, 0, len(subscriptions))
		for _, sub := range subscriptions {
			if !lo.ContainsBy(seen, func(item Subscription) bool { return item.Equal(sub) }) {
				seen = append(seen, sub)
			}
		}

		uniqueSubscriptions[localDomain] = seen
	}

	for localDomain, subscriptions := range uniqueSubscriptions {
		for _, subscription := range subscriptions {
			if !lo.ContainsBy(lo.Values(r.state.subs),
				func(item *Subscription) bool { return item.Equal(subscription) }) {

				// create subscription
				config, err := subscription.ToTargetConfig(localDomain)
				if err != nil {
					slog.Error("failed to build target config for subscription entry", "error", err)
					continue
				}

				id, err := r.targets.Create(config)
				if err != nil {
					slog.Error("failed to create target", "error", err)
				}

				r.state.subs[id] = &subscription
			}
		}
	}

	for name, mapping := range next.Domains {
		if !lo.ContainsBy(lo.Values(r.state.domains),
			func(item *DomainMapping) bool { return item.Equal(mapping) }) {
			id, err := r.domains.Add(mapping.URL, name, mapping.Node, mapping.Provider)
			if err != nil {
				slog.Error("failed to add domain mapping", "name", name, "error", err)
				continue
			}

			r.state.domains[id] = &mapping
		}
	}

	for id, existingSubscription := range r.state.subs {
		var found bool
		for _, subs := range uniqueSubscriptions {
			if lo.ContainsBy(subs,
				func(item Subscription) bool { return item.Equal(*existingSubscription) }) {
				found = true
				break
			}
		}

		if !found {
			delete(r.state.subs, id)
			r.targets.Destroy(id)
		}
	}

	for id, mapping := range r.state.domains {
		if !lo.ContainsBy(lo.Values(next.Domains), func(item DomainMapping) bool { return mapping.Equal(item) }) {
			r.domains.Remove(id)
			delete(r.state.domains, id)
		}
	}

	return nil
}
