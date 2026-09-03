VERSION := $(shell git describe --tags --dirty 2>/dev/null || echo unknown)
GO_LDFLAGS := -X github.com/jonasohland/mxl-replicator/internal/version.Version=$(VERSION)

# The legacy binaries are still built, because legacy/go stays in the tree until parity is
# declared and it is the production implementation until then (§16). Drop `proxy-legacy` and
# `reloader-legacy` from this list, and delete the tree, on the day parity is called.
.PHONY: all
all: cmake-configure cmake-build cmake-install tidy replicator proxy-legacy reloader-legacy

.PHONY: cmake-configure
cmake-configure:
	cmake -S . -B build -DCMAKE_INSTALL_PREFIX=build/install

.PHONY: cmake-build
cmake-build: cmake-configure
	cmake --build build -j

.PHONY: cmake-install
cmake-install: cmake-build
	cmake --install build

.PHONY: clean
clean:
	make -C build clean
	rm -f build/mxl-replicator build/mxl-fabrics-proxy build/mxl-fabrics-proxy-reloader

# Two modules: the root module (mxl-replicator) and the frozen legacy tree at
# legacy/go, tied together by go.work. `go mod tidy` is per-module.
.PHONY: tidy
tidy:
	go mod tidy
	go mod tidy -C legacy/go

.PHONY: test
test:
	go test ./...
	go test -C legacy/go ./...

.PHONY: replicator
replicator:
	go build -ldflags "$(GO_LDFLAGS)" -o build/mxl-replicator ./cmd/mxl-replicator

# The web UI, built into ui/app/dist, from where `ui/embed.go` compiles it into the binary.
#
# Deliberately not a prerequisite of `replicator` or of `all`: a Go build must not require a node
# toolchain, and the binary is complete without the UI — `--server-ui` is the flag that asks for it
# and it refuses to start when there is nothing behind it. Run this first, or `make replicator-ui`,
# when the assets are wanted.
.PHONY: ui
ui:
	npm --prefix ui/app ci
	npm --prefix ui/app run build

.PHONY: replicator-ui
replicator-ui: ui replicator

.PHONY: proxy-legacy
proxy-legacy:
	go build -o build/mxl-fabrics-proxy ./legacy/go/cmd/mxl-fabrics-proxy

.PHONY: reloader-legacy
reloader-legacy:
	go build -o build/mxl-fabrics-proxy-reloader ./legacy/go/cmd/mxl-fabrics-proxy-reloader

.PHONY: install
install: cmake-install

# Container images. `runtime` is what ships; `test` adds mxl-mock-src/mxl-mock-sink so the
# end-to-end suite can run against the same binaries, and must not be published as :latest.
IMAGE ?= jonasohland/mxl-replicator
EFA_BASE ?= jonasohland/mxl:v1.1-rc1-fabrics-efa

.PHONY: image
image:
	docker build --target runtime --build-arg VERSION=$(VERSION) -t $(IMAGE):latest .

.PHONY: image-efa
image-efa:
	docker build --target runtime --build-arg VERSION=$(VERSION) \
		--build-arg BASE_IMAGE=$(EFA_BASE) -t $(IMAGE):latest-efa .

.PHONY: image-test
image-test:
	docker build --target test --build-arg VERSION=$(VERSION) -t $(IMAGE):test .

# The Helm chart. `chart-lint` renders every example as well as the defaults, because most of what
# can break in this chart is a values combination rather than a template — the storage type, the
# EFA switch and the pool merge each take a different path through the same files.
CHART ?= deployment/mxl-replicator

.PHONY: chart-lint
chart-lint:
	helm lint $(CHART) --set server.persistence.node=lint
	for values in $(CHART)/examples/*.yaml; do \
		helm lint $(CHART) -f $$values || exit 1; \
	done

.PHONY: chart-template
chart-template:
	helm template mxl-replicator $(CHART) --set server.persistence.node=example

.PHONY: chart-package
chart-package:
	helm package $(CHART) -d build
