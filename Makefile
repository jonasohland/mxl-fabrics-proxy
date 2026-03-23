.PHONY: all
all: cmake-configure cmake-build cmake-install tidy proxy

.PHONY: cmake-configure
cmake-configure:
	cmake -S . -B build -DCMAKE_INSTALL_PREFIX=build/install

.PHONY: cmake-build
cmake-build: cmake-configure
	cmake --build build

.PHONY: cmake-install
cmake-install: cmake-build
	cmake --install build

.PHONY: clean
clean:
	make -C build clean
	rm -f build/mxl-fabrics-proxy

.PHONY: tidy
tidy:
	go mod tidy

.PHONY: proxy
proxy:
	go build -o build/mxl-fabrics-proxy ./go/cmd/mxl-fabrics-proxy
