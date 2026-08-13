.PHONY: all
all: cmake-configure cmake-build cmake-install tidy proxy reloader

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

.PHONY: tidy
tidy:
	go mod tidy

.PHONY: proxy
proxy:
	go build -o build/mxl-fabrics-proxy ./go/cmd/mxl-fabrics-proxy

.PHONY: reloader
reloader:
	go build -o build/mxl-fabrics-proxy-reloader ./go/cmd/mxl-fabrics-proxy-reloader

.PHONY: install
install: cmake-install
