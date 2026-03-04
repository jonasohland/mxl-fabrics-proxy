

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

} // namespace

namespace mxl::proxy {
Config Config::read(nlohmann::json config) {
    Config out{};

    out.node = getString(config, "node");
    out.service = getString(config, "service");
    out.targetInfo = getString(config, "target_info");
    out.flowDef = getString(config, "flow_def", "");
    out.flowId = getString(config, "flow_id", "");
    out.domain = getString(config, "domain");
    out.providerStr = getString(config, "provider", "tcp");

    ::mxl::mxl(::mxlFabricsProviderFromString,
               "failed to parse provider string", out.providerStr.c_str(),
               &out.provider);

    if (out.flowId.empty() && out.flowDef.empty()) {
        throw Exception{MXL_ERR_INVALID_ARG,
                        "one of 'flow_id' or 'flow_def' must be specified"};
    }

    if ((!out.flowId.empty()) && (!out.flowDef.empty())) {
        throw Exception{
            MXL_ERR_INVALID_ARG,
            "only one of 'flow_id' or 'flow_def' must be specified"};
    }

    return out;
}

bool Config::isTarget() const noexcept {
    return !flowDef.empty();
}

bool Config::isInitiator() const noexcept {
    return !flowId.empty();
}

} // namespace mxl::proxy
