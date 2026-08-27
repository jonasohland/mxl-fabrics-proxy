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

.PHONY: tidy
tidy:
	go mod tidy

.PHONY: proxy-legacy
proxy-legacy:
	go build -o build/mxl-fabrics-proxy ./legacy/go/cmd/mxl-fabrics-proxy

.PHONY: reloader-legacy
reloader-legacy:
	go build -o build/mxl-fabrics-proxy-reloader ./legacy/go/cmd/mxl-fabrics-proxy-reloader

.PHONY: install
install: cmake-install
