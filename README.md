# mxl-fabrics-proxy

[![Docker Image](https://img.shields.io/badge/docker-jonasohland%2Fmxl--fabrics--proxy-blue?logo=docker)](https://hub.docker.com/r/jonasohland/mxl-fabrics-proxy)
[![Go](https://img.shields.io/badge/Go-1.26+-00ADD8?logo=go)](https://golang.org)
[![License](https://img.shields.io/badge/license-MIT-green)](LICENSE)

## Disclaimer

**This project is currently considered experimental.**

The `mxl-fabrics-proxy` is provided as-is for evaluation and testing purposes. While it is already being used for example as the central inter-host replication app for the [AWS MXL Interop at NAB 2026](https://www.linkedin.com/posts/tgedwards_come-see-media-exchange-layer-mxl-sdk-interop-share-7452418229056012288-FV2a/), the configuration format, HTTP API, and subscription model are subject to change without notice.

This is intentional: active work is underway on a **standard discovery and connection API** for mxl-enabled media functions. Once that standard matures, this proxy will be updated to align with it. Backward compatibility will be maintained where possible, but breaking changes to the configuration schema or API endpoints may occur in future releases.

**Use in production at your own risk.** We recommend pinning to a specific Docker tag and testing configuration reloads thoroughly before relying on them in critical workflows.

## Overview

`mxl-fabrics-proxy` enables efficient, low-latency, uncompressed video (and other high-bandwidth flows) to be transferred between hosts with **minimal CPU and memory overhead**. It leverages RDMA-capable network interfaces (verbs, EFA, shm, tcp) via the mxl-fabrics stack.

It acts as a lightweight proxy that:
- Exposes local MXL domains to remote peers
- Subscribes to remote MXL flows and maps them into local domains
- Supports dynamic configuration reload without restarting the process and impacting running flows.

## Features

- **RDMA-accelerated transfers** with libfabric providers (`tcp`, `verbs`, `shm`, `efa`)
- **AWS EFA optimized** Docker image with Amazon's tuned libfabric build
- **Near-zero-copy** path for uncompressed video
- **Dynamic configuration** via YAML file + HTTP reload endpoint
- **Rich observability** — Prometheus metrics + structured logfmt tracing

## Docker Images

Two variants are published:

| Image                                      | Description                              | Recommended for          |
|--------------------------------------------|------------------------------------------|--------------------------|
| `jonasohland/mxl-fabrics-proxy:latest`     | Regular libfabric build                  | On-prem / standard NICs  |
| `jonasohland/mxl-fabrics-proxy:latest-efa` | AWS EFA-optimized libfabric build        | AWS EC2 with EFA         |

```bash
# Regular
docker pull jonasohland/mxl-fabrics-proxy:latest

# EFA optimized
docker pull jonasohland/mxl-fabrics-proxy:latest-efa
```
## Quick Start

```bash
docker run -d \
  --name mxl-proxy \
  -p 2283:2283 \
  -v /dev/shm:/dev/shm \
  -v $(pwd):/config \
  jonasohland/mxl-fabrics-proxy:latest \
  --config /config/config.yaml \
  --listen 0.0.0.0:2283
```

## Configuration

The proxy can be configured entirely via **command-line flags** or a **YAML configuration file** (recommended for complex setups). CLI flags and config file values are merged (CLI takes precedence for simple fields).

### Command-Line Flags

```bash
mxl-fabrics-proxy [flags]
```

| Flag                    | Short | Default              | Description |
|-------------------------|-------|----------------------|-----------|
| `--config`              |       | —                    | Path to YAML config file(s) or directories (can be repeated) |
| `--listen`              | `-l`  | `127.0.0.1:2283`     | HTTP listen address for API & metrics |
| `--node`                |       | `127.0.0.1`          | Local node address (provider dependent) |
| `--log-level`           |       | `info`               | `debug`, `info`, `warn`, `error` |
| `--tracing`             | `-t`  | `false`              | Enable verbose logfmt tracing (flow labels, etc.) |
| `--efa-use-wait`        |       | `false`              | Enable wait objects with EFA provider |
| `--domain`              | `-d`  | —                    | Add local domain (repeatable) |
| `--map-domain`          | `-m`  | —                    | Domain alias mapping |
| `--subscribe`           | `-s`  | —                    | Add subscription (repeatable) |

Example of a domain mapping on the command line:
```sh
-m domain-1=/dev/shm/domain-1
```

Example of a subscription on the command line:
```sh
# <local-domain-mapped-name>@<remote-flow-url>
-s "domain-1@mxl://remote-engine.dc1.company.net?id=5592a23b-0974-45bb-9388-89ea81c42537&provider=verbs"
```

Complete example looping back a flow from /dev/shm/mxl0 to /dev/shm/mxl1
```sh
mxl-fabrics-proxy --node 127.0.0.1 \
    -m mxl0=/dev/shm/mxl0 \
    -m mxl1=/dev/shm/mxl1 \
    -s "mxl1@mxl://127.0.0.1/dev/shm/mxl0?id=5592a23b-0974-45bb-9388-89ea81c42537&provider=tcp"
```

### Configuration File (`config.yaml`)

The configuration file supports three main sections (plus an optional `remotes` section for advanced aliasing):

```yaml
# Defaults applied to all domains and subscriptions
defaults:
  node: 127.0.0.1
  provider: tcp                 # tcp | verbs | shm | efa
  tracing: true                 # lots of logfmt output
  labels:
    local: mxl111
  # Optional performance tuning
  # no_network_latency_measurement: false
  # sched_prio: 0

# Local domains exposed by this proxy
domains:
  mxl0:
    url: mxl:///dev/shm/in0
    labels:
      xxx: in0
  loopback1:
    url: mxl:///dev/shm/loopback1

# Subscriptions from remote proxies into local domains
subscriptions:
  in0:
    # Full URL with flow ID
    - url: mxl://remote.my.org:2283/dev/shm/mxl1?id=e929dbcf-1710-4e32-b94f-5345928eb4aa

    # Using remote domain alias (defined in the *remote* proxy's domains section)
    - url: mxl://remote.my.org/mxl1
      id: e929dbcf-1710-4e32-b94f-5345928eb4aa

    # Multiple flow IDs
    - url: mxl://remote.my.org/mxl1
      ids:
        - e929dbcf-1710-4e32-b94f-5345928eb4aa
        - another-flow-uuid

    # Loopback / local testing
    - url: mxl://localhost/loopback1?id=bdb0229d-b2cb-42e1-8d33-b1f906d35186
```

#### Optional Advanced Section: `remotes`

For cleaner configuration when talking to many remote proxies:

```yaml
remotes:
  production-east:
    endpoint: remote-east.my.org:2283
    provider: efa
    domains:
      main: /dev/shm/mxl1
      backup: /dev/shm/mxl2
    labels:
      region: us-east-1

subscriptions:
  in0:
    - remote: production-east/main
      id: e929dbcf-1710-4e32-b94f-5345928eb4aa
```

### Reloading Configuration

Send a `POST` request to reload the configuration at runtime (no restart required):

```bash
curl -X POST http://localhost:2283/v1/reload
```

The proxy will atomically apply the new configuration while keeping existing flows running where possible.

## Building from Source

```bash
git clone https://github.com/jonasohland/mxl-fabrics-proxy.git
cd mxl-fabrics-proxy

# Build (requires CMake + Go 1.26+)
make

# Or manually
mkdir build && cd build
cmake .. -DCMAKE_BUILD_TYPE=Release
make -j$(nproc)
```

The resulting binaries are:
- `mxl-fabrics-proxy` — main proxy
- `mxl-fabrics-proxy-worker` — internal worker (launched automatically)

## Observability

- **Metrics**: Prometheus endpoint at `http://<listen>/metrics`
- **Tracing**: Enable with `--tracing` or `tracing: true` in config for detailed logfmt output including flow labels and descriptions

## Contributing

Contributions are welcome! Please open an issue or pull request.

## License

Apache-2.0 License — see [LICENSE](LICENSE) for details.
