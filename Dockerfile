ARG BASE_IMAGE="jonasohland/mxl:v1.1-rc1-fabrics"
FROM ${BASE_IMAGE} AS builder

USER 0:0

RUN apt-get update && \
    apt-get install -y \
        build-essential \
        cmake \
        pkg-config \
        libspdlog-dev \
        picojson-dev \
        nlohmann-json3-dev \
        uuid-dev

RUN curl -LO https://go.dev/dl/go1.26.1.linux-amd64.tar.gz && \
    tar -C /usr/local -xzf go1.26.1.linux-amd64.tar.gz

WORKDIR /build/mxl-replicator

# The C++ half: the worker, plus mxl-mock-src/mxl-mock-sink, which the test image below copies
# out and the runtime image deliberately does not.
COPY CMakeLists.txt CMakeLists.txt
COPY src src

RUN PKG_CONFIG_PATH=/opt/amazon/efa/lib/pkgconfig cmake -B build  \
    -DCMAKE_BUILD_TYPE=Release \
    -DCMAKE_INSTALL_PREFIX=/usr \
    .

RUN make -C build -j$(nproc)
RUN make -C build install

# The Go half. go.work ties the root module to the frozen legacy tree at legacy/go, and the legacy
# tree is not in this image — so build with the workspace off rather than copying a module this
# image has no use for (§16: legacy/go stays in the repository until parity, not in the image).
COPY go.mod go.sum ./
RUN GOWORK=off /usr/local/go/bin/go mod download

COPY cmd cmd
COPY internal internal

ARG VERSION="unknown"
RUN GOWORK=off /usr/local/go/bin/go build \
        -ldflags "-X github.com/jonasohland/mxl-replicator/internal/version.Version=${VERSION}" \
        -o /usr/bin/mxl-replicator ./cmd/mxl-replicator

FROM ${BASE_IMAGE} AS runtime

USER 0:0
RUN apt-get update && \
    apt-get install -y \
        libspdlog1.15 \
        libuuid1 \
        dumb-init \
        && rm -r /var/lib/apt/lists/*

COPY --from=builder /usr/bin/mxl-replicator /usr/bin/mxl-replicator
COPY --from=builder /usr/bin/mxl-replicator-worker /usr/bin/mxl-replicator-worker

# The agent's per-worker work directories. Each holds a config.json, a metrics socket and, for a
# target, a target-info.json; a fresh one per worker *start*, because the worker does not unlink a
# pre-existing metrics socket before binding and a leftover from a SIGKILL is a fatal EADDRINUSE
# (§6). Keep the path short: it holds an AF_UNIX socket, which is capped at 108 bytes.
RUN mkdir -p /run/mxl-replicator && chown mxl:mxl /run/mxl-replicator

# No node-address snippet script and no reloader. The first is superseded by the interface probe,
# which asks libfabric what this node actually has instead of guessing from the pod's environment
# (§10.5); the second existed to reload subscriptions from a file, and that state now lives in the
# API (§6.1).

USER mxl:mxl

# The reference deployment is a DaemonSet with one agent per node and the workers as its children,
# so a container restart takes the PID namespace and every worker with it — which is why the agent
# holds no state and re-establishes rather than adopting (§6.1).
ENV GOMAXPROCS=1
ENTRYPOINT ["/usr/bin/dumb-init", "--", "/usr/bin/mxl-replicator"]
CMD ["run"]

# The test image adds the mock producer and consumer, so the end-to-end suite can run against the
# same binaries that ship. They stay out of `runtime` on purpose: a production image should not
# carry tools that write into a domain.
FROM runtime AS test

USER 0:0
COPY --from=builder /usr/bin/mxl-mock-src /usr/bin/mxl-mock-src
COPY --from=builder /usr/bin/mxl-mock-sink /usr/bin/mxl-mock-sink
USER mxl:mxl
