

#include "config.hpp"
#include "mxl.hpp"
#include <mxl/fabrics.h>

namespace {

std::string getString(nlohmann::json const& v, std::string_view key) {
    if (auto it = v.find(key); it != v.end() && it->is_string()) {
        return it->get<std::string>();
    }
    throw mxl::Exception{MXL_ERR_INVALID_ARG,
                         std::format("missing required field: {}", key)};
}

std::string getString(nlohmann::json const& v, std::string_view key,
                      std::string_view def) {
    if (auto it = v.find(key); it != v.end() && it->is_string()) {
        return it->get<std::string>();
    }
    return std::string{def};
}

bool getBool(nlohmann::json const& v, std::string_view key) {
    if (auto it = v.find(key); it != v.end() && it->is_boolean()) {
        return it->get<bool>();
    }
    return false;
}

} // namespace

namespace mxl::proxy {
Config Config::read(nlohmann::json config) {
    Config out{};

    out.target = getBool(config, "target");
    out.metricsSocket = getString(config, "metrics_socket");
    out.node = getString(config, "node");
    out.service = getString(config, "service");
    out.targetInfo = getString(config, "target_info");
    out.flowDef = getString(config, "flow_def", "");
    out.flowId = getString(config, "flow_id", "");
    out.domain = getString(config, "domain");
    out.providerStr = getString(config, "provider", "tcp");
    out.noNetworkLatencyMeasurement =
        getBool(config, "no_network_latency_measurement");

    ::mxl::mxl(::mxlFabricsProviderFromString,
               "failed to parse provider string", out.providerStr.c_str(),
               &out.provider);

    return out;
}

bool Config::isTarget() const noexcept {
    return target;
}

bool Config::isInitiator() const noexcept {
    return !target;
}

} // namespace mxl::proxy
