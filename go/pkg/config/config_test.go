package config_test

import (
	"testing"

	"github.com/jonasohland/mxl-fabrics-proxy/go/pkg/config"
	"github.com/kr/pretty"
	"github.com/stretchr/testify/require"
)

type Test struct {
	Val string
}

func TestConfigNormalize(t *testing.T) {
	cfg := config.Config{
		Domains: map[string]config.DomainMapping{
			"local0": {
				URL:      "mxl:///dev/shm/local",
				Node:     "test1234",
				Provider: "verbs",
			},
		},
		Remotes: map[string]config.Remote{
			"remote_without_provider": {
				Endpoint: "no-provider.org",
				Domains: map[string]string{
					"np": "/np",
				},
			},
			"1234": {
				Endpoint: "engine01.dc0.mycompany.org",
				Provider: "efa",
				Domains: map[string]string{
					"d0": "/dev/shm/0",
					"d1": "/dev/shm/1",
				},
			},
		},
		Subscriptions: map[string][]config.Subscription{
			"mxl:///dev/shm/in": {
				{
					Remote: "1234/d0",
					IDs:    []string{"1234", "5678"},
				},
			},
			"local0": {
				{
					Remote: "1234/d1",
					IDs:    []string{"000"},
				},
				{
					URL: "mxl://engine14.mycompany.org/dev/shm/1234?id=xxxx&id=yyyy",
				},
				{
					URL:      "mxl://engine14.mycompany.org/dev/shm/xxxx?id=xxxx&id=yyyy",
					Provider: "shm",
				},
			},
			"mxl:///no-provider": {
				{
					Remote: "remote_without_provider/np",
					ID:     "xxxx",
				},
				{
					Remote:   "remote_without_provider/np",
					Provider: "shm",
					ID:       "yyyy",
				},
			},
		},
	}

	require.NoError(t, cfg.Normalize())
	_, _ = pretty.Println(cfg)
}
