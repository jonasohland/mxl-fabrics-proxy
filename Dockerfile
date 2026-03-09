ARG BASE_IMAGE="jonasohland/mxl:e4a8646-fabrics"
FROM ${BASE_IMAGE} AS builder

USER 0:0

RUN apt-get update && \
    apt-get install -y \
        build-essential \
        cmake \
        pkg-config \
        libspdlog-dev \
        picojson-dev \
        nlohmann-json3-dev

RUN curl -LO https://go.dev/dl/go1.26.1.linux-amd64.tar.gz && \
    tar -C /usr/local -xzf go1.26.1.linux-amd64.tar.gz

WORKDIR /build/mxl-fabrics-proxy

COPY CMakeLists.txt CMakeLists.txt
COPY src src
COPY go go
COPY go.mod go.mod
COPY go.sum go.sum

RUN PKG_CONFIG_PATH=/opt/amazon/efa/lib/pkgconfig cmake -B build  \
    -DCMAKE_BUILD_TYPE=Release \
    -DCMAKE_INSTALL_PREFIX=/usr \
    .

RUN make -C build -j$(nproc)
RUN make -C build install
RUN /usr/local/go/bin/go build -o /usr/bin/mxl-fabrics-proxy ./go/cmd/mxl-fabrics-proxy

FROM ${BASE_IMAGE}

USER 0:0
RUN apt-get update && \
    apt-get install -y \
        libspdlog1.15 \
        dumb-init \
        && rm -r /var/lib/apt/lists/* 

COPY --from=builder /usr/bin/mxl-fabrics-proxy /usr/bin/mxl-fabrics-proxy
COPY --from=builder /usr/bin/mxl-fabrics-proxy-worker /usr/bin/mxl-fabrics-proxy-worker

USER mxl:mxl
ENV GOMAXPROCS=1
ENTRYPOINT ["/usr/bin/dumb-init", "--", "/usr/bin/mxl-fabrics-proxy"]
