#! /bin/bash

set -e

base_image="jonasohland/mxl:2e2ad13-fabrics"

project_dir="$(realpath "$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" &>/dev/null && pwd)"/..)"
version="$(git -C "${project_dir}" describe --tags)"

docker build --build-arg "BASE_IMAGE=${base_image}" -t "jonasohland/mxl-fabrics-proxy:${version}" "${project_dir}"
docker build --build-arg "BASE_IMAGE=${base_image}-efa" -t "jonasohland/mxl-fabrics-proxy:${version}-efa" "${project_dir}"
