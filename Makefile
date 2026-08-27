.PHONY: all
all: cmake-configure cmake-build cmake-install tidy proxy-legacy reloader-legacy

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
	rm -f build/mxl-fabrics-proxy build/mxl-fabrics-proxy-reloader

# Two modules: the root module (mxl-replicator) and the frozen legacy tree at
# legacy/go, tied together by go.work. `go mod tidy` is per-module.
.PHONY: tidy
tidy:
	go mod tidy
	go mod tidy -C legacy/go

# The root module has no packages yet; add `go test ./...` here in M1.
.PHONY: test
test:
	go test -C legacy/go ./...

.PHONY: proxy-legacy
proxy-legacy:
	go build -o build/mxl-fabrics-proxy ./legacy/go/cmd/mxl-fabrics-proxy

.PHONY: reloader-legacy
reloader-legacy:
	go build -o build/mxl-fabrics-proxy-reloader ./legacy/go/cmd/mxl-fabrics-proxy-reloader

.PHONY: install
install: cmake-install
