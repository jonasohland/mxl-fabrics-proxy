#! /bin/bash
set -euo pipefail

node_name="${MXL_FABRICS_PROXY_NODE_NAME:-}"
provider="${MXL_FABRICS_PROVIDER:-}"
config_dir="${MXL_FABRICS_PROXY_CONFIG_DIR:-/config}"
node_config="${MXL_FABRICS_PROXY_NODE_CONFIG:-/tmp/mxl-fabrics-proxy/node.yaml}"

fail() {
  echo "$(basename "$0"): $*" >&2
  exit 1
}

[[ -n "$node_name" ]] || fail "MXL_FABRICS_PROXY_NODE_NAME is not set"
[[ -n "$provider" ]] || fail "MXL_FABRICS_PROVIDER is not set"

config=""
for candidate in "$config_dir/$node_name.yaml" "$config_dir/$node_name.yml"; do
  if [[ -f "$candidate" ]]; then
    config="$candidate"
    break
  fi
done

[[ -n "$config" ]] || fail "no configuration for node '$node_name' in '$config_dir'"

echo "starting proxy for node '$node_name' with '$config'" >&2

exec dumb-init -- mxl-fabrics-proxy \
  --config "$config" \
  --config "$node_config" \
  "$@"
