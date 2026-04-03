package config

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/url"
	"os"
	"slices"
	"strings"

	"github.com/jonasohland/mxl-fabrics-proxy/go/pkg/target"
	"github.com/samber/lo"
	"sigs.k8s.io/yaml"
)

var (
	ErrInvalidSubscriptionFormatting = errors.New("invalid request format")
	ErrMissingDomainMapping          = errors.New("missing domain mapping")
	ErrInvalidSubscription           = errors.New("invalid subscription spec")
	ErrRemoteNotFound                = errors.New("remote not found")
	ErrInvalidRemoteReferenceFormat  = errors.New("invalid remote domain reference format")
	ErrRemoteDomainNotFound          = errors.New("remote domain not found")
)

type DomainMapping struct {
	URL      string            `json:"url"`
	Node     string            `json:"node"`
	Provider string            `json:"provider"`
	Labels   map[string]string `json:"labels"`
}

func (d *DomainMapping) Equal(other DomainMapping) bool {
	return d.URL == other.URL
}

type Remote struct {
	Endpoint string            `json:"endpoint"`
	Provider string            `json:"provider"`
	Node     string            `json:"node"`
	Domains  map[string]string `json:"domains"`
	Labels   map[string]string `json:"labels"`
}

type Subscription struct {
	URL             string            `json:"url"`
	Remote          string            `json:"remote"`
	IDs             []string          `json:"ids"`
	ID              string            `json:"id"`
	Provider        string            `json:"provider"`
	Node            string            `json:"node"`
	Labels          map[string]string `json:"labels"`
	NoNetLatMeasure bool              `json:"no_network_latency_measurement"`
	SchedPrio       *int              `json:"sched_prio"`

	localDomain string
}

func (s *Subscription) Equal(other Subscription) bool {
	return s.URL == other.URL &&
		s.Remote == other.Remote &&
		slices.Equal(s.IDs, other.IDs) &&
		s.ID == other.ID &&
		s.Provider == other.Provider &&
		s.Node == other.Node &&
		maps.Equal(s.Labels, other.Labels) &&
		s.NoNetLatMeasure == other.NoNetLatMeasure &&
		((s.SchedPrio == nil && other.SchedPrio == nil) ||
			s.SchedPrio != nil && other.SchedPrio != nil && *s.SchedPrio == *other.SchedPrio) &&
		s.localDomain == other.localDomain
}

func (s *Subscription) ToTargetConfig(localDomain string, tracing bool) (*target.TargetConfig, error) {
	durl, err := url.Parse(localDomain)
	if err != nil {
		return nil, err
	}

	rurl, err := url.Parse(s.URL)
	if err != nil {
		return nil, err
	}

	host := rurl.Host
	if len(strings.Split(host, ":")) < 2 {
		host += ":2283"
	}

	return &target.TargetConfig{
		LocalDomainPath:  durl.Path,
		RemoteDomainPath: rurl.Path,
		RemoteHost:       host,
		Provider:         s.Provider,
		Node:             s.Node,
		FlowID:           s.ID,
		Labels:           s.Labels,
		NoNetLatMeasure:  s.NoNetLatMeasure,
		SchedPrio:        s.SchedPrio,
		Tracing:          tracing,
	}, nil
}

type Defaults struct {
	Node            string            `json:"node"`
	Provider        string            `json:"provider"`
	NoNetLatMeasure bool              `json:"no_network_latency_measurement"`
	SchedPrio       *int              `json:"sched_prio"`
	Labels          map[string]string `json:"labels"`
}

type state struct {
	subs    map[string]*Subscription
	domains map[string]*DomainMapping
}

type Config struct {
	Defaults      Defaults                  `json:"defaults"`
	Remotes       map[string]Remote         `json:"remotes"`
	Domains       map[string]DomainMapping  `json:"domains"`
	Subscriptions map[string][]Subscription `json:"subscriptions"`
}

func (c *Config) Normalize() error {
	if err := c.normalizeSubscriptions(); err != nil {
		return err
	}

	return c.normalizeDomains()
}

func (c *Config) normalizeDomains() error {
	for name, domain := range c.Domains {
		if domain.Node == "" {
			domain.Node = c.Defaults.Node
		}

		c.Domains[name] = domain
	}

	return nil
}

func (c *Config) normalizeSubscriptions() error {
	normalized := map[string][]Subscription{}
	for domain, subscriptions := range c.Subscriptions {
		if !strings.HasPrefix(domain, "mxl://") {
			resolved, ok := c.Domains[domain]
			if !ok {
				return fmt.Errorf("resolve domain '%s': %w", domain, ErrMissingDomainMapping)
			}

			domain = resolved.URL
		}

		for _, subscription := range subscriptions {
			subscriptions, err := c.normalizeSubscription(domain, &subscription)
			if err != nil {
				return fmt.Errorf("failed to normalize subscription config for domain '%s', url '%s': %w",
					domain, subscription.ID, err)
			}

			normalized[domain] = append(normalized[domain], subscriptions...)
		}
	}

	c.Subscriptions = normalized

	return nil
}

func (c *Config) resolveProvider(localDomain string, remote string) string {
	for _, mapping := range c.Domains {
		if mapping.URL == localDomain {
			if mapping.Provider != "" {
				return mapping.Provider
			}
			break
		}
	}

	mapping, ok := c.Domains[localDomain]
	if ok {
		if mapping.Provider != "" {
			return mapping.Provider
		}
	}

	rem, ok := c.Remotes[remote]
	if ok {
		if rem.Provider != "" {
			return rem.Provider
		}
	}

	if c.Defaults.Provider != "" {
		return c.Defaults.Provider
	}

	return "tcp"
}

func (c *Config) resolveNode(localDomain string, remote string) string {
	for _, mapping := range c.Domains {
		if mapping.URL == localDomain {
			if mapping.Node != "" {
				return mapping.Node
			}
			break
		}
	}

	mapping, ok := c.Domains[localDomain]
	if ok {
		if mapping.Node != "" {
			return mapping.Node
		}
	}

	rem, ok := c.Remotes[remote]
	if ok {
		if rem.Node != "" {
			return rem.Node
		}
	}

	if c.Defaults.Node != "" {
		return c.Defaults.Node
	}

	return ""
}

func (c *Config) normalizeSubscription(domain string, sub *Subscription) ([]Subscription, error) {
	sub.localDomain = domain
	if sub.Remote != "" {
		return c.normalizeSubscriptionWithRemote(domain, sub)
	} else {
		return c.normalizeSubscriptionWithURL(domain, sub)
	}
}

func (c *Config) orDefaultSchedPrio(v *int) *int {
	if v == nil && c.Defaults.SchedPrio != nil {
		return c.Defaults.SchedPrio
	}

	return v
}

func (c *Config) getLabels(domainURL string, remoteName string) map[string]string {
	labels := c.Defaults.Labels

	for _, domain := range c.Domains {
		if domain.URL == domainURL {
			labels = lo.Assign(labels, domain.Labels)
			break
		}
	}

	remoteObj, ok := c.Remotes[remoteName]
	if ok {
		labels = lo.Assign(labels, remoteObj.Labels)
	}

	return labels
}

func (c *Config) normalizeSubscriptionWithRemote(domain string, sub *Subscription) ([]Subscription, error) {
	if sub.URL != "" {
		return nil, fmt.Errorf("%w: cannot have both url and remote field", ErrInvalidSubscription)
	}

	remoteParts := strings.SplitN(sub.Remote, "/", 2)
	if len(remoteParts) != 2 {
		return nil, fmt.Errorf("parse '%s': %w", sub.Remote, ErrInvalidRemoteReferenceFormat)
	}

	remoteName, remoteDomainName := remoteParts[0], remoteParts[1]
	remote, ok := c.Remotes[remoteName]
	if !ok {
		return nil, fmt.Errorf("get '%s': %w", sub.Remote, ErrRemoteNotFound)
	}

	remoteDomainPath, ok := remote.Domains[remoteDomainName]
	if !ok {
		return nil, fmt.Errorf("resolve remote domain '%s': %w", remoteDomainName, ErrRemoteDomainNotFound)
	}

	remoteDomainURL := fmt.Sprintf("mxl://%s%s", remote.Endpoint, remoteDomainPath)

	if sub.Provider == "" {
		sub.Provider = c.resolveProvider(domain, remoteName)
	}
	if sub.Node == "" {
		sub.Node = c.resolveNode(domain, remoteName)
	}

	ids := sub.IDs
	if sub.ID != "" {
		ids = append(ids, sub.ID)
	}

	return lo.Map(ids,
		func(id string, _ int) Subscription {
			return Subscription{
				URL:             remoteDomainURL,
				ID:              id,
				Provider:        sub.Provider,
				Node:            sub.Node,
				Labels:          lo.Assign(c.getLabels(domain, remoteName), sub.Labels),
				NoNetLatMeasure: sub.NoNetLatMeasure || c.Defaults.NoNetLatMeasure,
				SchedPrio:       c.orDefaultSchedPrio(sub.SchedPrio),

				localDomain: sub.localDomain,
			}
		}), nil
}

func (c *Config) normalizeSubscriptionWithURL(domain string, sub *Subscription) ([]Subscription, error) {
	durl, err := url.Parse(sub.URL)
	if err != nil {
		return nil, fmt.Errorf("parse subscription remote url: %w", err)
	}

	if durl.Scheme != "mxl" {
		return nil, fmt.Errorf("invalid url scheme '%s'", durl.Scheme)
	}

	sanitizedURL := "mxl://" + durl.Host + durl.Path

	if sub.Provider == "" {
		sub.Provider = c.resolveProvider(domain, "")
	}
	if sub.Node == "" {
		sub.Node = c.resolveNode(domain, "")
	}

	var ids []string
	ids = append(ids, durl.Query()["id"]...)
	ids = append(ids, sub.IDs...)
	if sub.ID != "" {
		ids = append(ids, sub.ID)
	}

	return lo.Map(lo.Uniq(ids),
		func(id string, _ int) Subscription {
			return Subscription{
				URL:             sanitizedURL,
				ID:              id,
				Provider:        sub.Provider,
				Node:            sub.Node,
				Labels:          lo.Assign(c.getLabels(domain, ""), sub.Labels),
				NoNetLatMeasure: sub.NoNetLatMeasure || c.Defaults.NoNetLatMeasure,
				SchedPrio:       c.orDefaultSchedPrio(sub.SchedPrio),

				localDomain: sub.localDomain,
			}
		},
	), nil
}

func (c *Config) AddDomainFromString(domainURL string) error {
	name := strings.ReplaceAll(strings.TrimPrefix(domainURL, "mxl://"), "/", "-")
	return c.AddDomainMappingFromString(name, domainURL)
}

func (c *Config) AddDomainMappingFromString(name, domainURL string) error {
	durl, err := url.Parse(domainURL)
	if err != nil {
		return err
	}

	sanitizedURL := "mxl://" + durl.Host + durl.Path

	c.Domains[name] = DomainMapping{
		URL:      sanitizedURL,
		Node:     durl.Query().Get("node"),
		Provider: durl.Query().Get("provider"),
	}

	return nil
}

func (c *Config) AddRequestFromString(req string) error {
	parts := strings.Split(req, "@")
	if len(parts) != 2 {
		return ErrInvalidSubscriptionFormatting
	}

	localDomain, remoteDomainAndFlow := parts[0], parts[1]

	rurl, err := url.Parse(remoteDomainAndFlow)
	if err != nil {
		return err
	}

	sanitizedURL := "mxl://" + rurl.Host + rurl.Path

	provider := c.resolveProvider(localDomain, "")
	if rurl.Query().Get("provider") != "" {
		provider = rurl.Query().Get("provider")
	}

	for _, id := range rurl.Query()["id"] {
		c.Subscriptions[localDomain] = append(
			c.Subscriptions[localDomain],
			Subscription{
				URL:      sanitizedURL,
				ID:       id,
				Provider: provider,
			},
		)
	}

	return nil
}

func (c *Config) merge(other *Config) {
	if c.Domains == nil {
		c.Domains = map[string]DomainMapping{}
	}
	if c.Subscriptions == nil {
		c.Subscriptions = map[string][]Subscription{}
	}
	if c.Remotes == nil {
		c.Remotes = map[string]Remote{}
	}
	c.Defaults.Labels = lo.Assign(c.Defaults.Labels, other.Defaults.Labels)

	if other.Defaults.Node != "" {
		if c.Defaults.Node != "" && c.Defaults.Node != other.Defaults.Node {
			slog.Warn("default node specified multiple times")
		}

		c.Defaults.Node = other.Defaults.Node
	}
	if other.Defaults.Provider != "" {
		if c.Defaults.Provider != "" && c.Defaults.Provider != other.Defaults.Provider {
			slog.Warn("default provider specified multiple times")
		}

		c.Defaults.Provider = other.Defaults.Provider
	}

	c.Defaults.NoNetLatMeasure = c.Defaults.NoNetLatMeasure || other.Defaults.NoNetLatMeasure

	for name, domain := range other.Domains {
		_, ok := c.Domains[name]
		if ok {
			slog.Info("overriding existing domain mapping", "name", name)
		}

		c.Domains[name] = domain
	}

	for name, remote := range other.Remotes {
		_, ok := c.Remotes[name]
		if ok {
			slog.Info("overriding existing remote spec", "name", name)
		}

		c.Remotes[name] = remote
	}

	for localDomain, subscriptions := range other.Subscriptions {
		c.Subscriptions[localDomain] = append(c.Subscriptions[localDomain], subscriptions...)
	}

	if other.Defaults.SchedPrio != nil {
		c.Defaults.SchedPrio = other.Defaults.SchedPrio
	}
}

func Load(filename string) (*Config, error) {
	fd, err := os.OpenFile(filename, os.O_RDONLY, 0000)
	if err != nil {
		return nil, err
	}
	defer func() { _ = fd.Close() }()

	var config Config
	buf, err := io.ReadAll(fd)
	if err != nil {
		return nil, err
	}

	if err := yaml.Unmarshal(buf, &config); err != nil {
		return nil, err
	}

	return &config, nil
}
