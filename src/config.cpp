

#include "config.hpp"
#include "fabrics.hpp"
#include "mxl.hpp"
#include <mxl/fabrics.h>

namespace {

int getInt(nlohmann::json const& v, std::string_view key, int def) {
    if (auto it = v.find(key); it != v.end() && it->is_number()) {
        return it->get<int>();
    }

    return def;
}

std::uint64_t getUint64(nlohmann::json const& v, std::string_view key,
                        std::uint64_t def) {
    if (auto it = v.find(key); it != v.end() && it->is_number_unsigned()) {
        return it->get<std::uint64_t>();
    }

    return def;
}

/// Read a millisecond duration. A value of 0 or less means "wait
/// indefinitely", which is std::nullopt here — every use site then has to
/// decide what to do about the absence rather than comparing against a
/// sentinel that looks like a duration.
std::optional<std::chrono::milliseconds>
getTimeout(nlohmann::json const& v, std::string_view key, int defMs) {
    auto const ms = getInt(v, key, defMs);
    if (ms <= 0) {
        return std::nullopt;
    }
    return std::chrono::milliseconds{ms};
}

/// Capability flags as an array of names, e.g. ["REMOTE_WRITE",
/// "BLOCKING_OPERATIONS"]. Names rather than a bitmask because the same
/// spelling appears in the --interfaces output, so whatever agreed the config
/// never has to translate between two representations.
std::uint64_t getCapsFlags(nlohmann::json const& v, std::string_view key,
                           std::uint64_t def) {
    auto it = v.find(key);
    if (it == v.end() || it->is_null()) {
        return def;
    }
    if (!it->is_array()) {
        throw mxl::Exception{
            MXL_ERR_INVALID_ARG,
            std::format("{} must be an array of capability flag names", key)};
    }

    auto flags = std::uint64_t{0};
    for (auto const& entry : *it) {
        if (!entry.is_string()) {
            throw mxl::Exception{
                MXL_ERR_INVALID_ARG,
                std::format("{} must be an array of capability flag names",
                            key)};
        }
        flags |=
            mxl::fabrics::capsFlagFromName(entry.get_ref<std::string const&>());
    }
    return flags;
}

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
    out.schedPrio =
        getInt(config, "sched_prio", std::numeric_limits<int>::min());

    // The default reproduces what was hardcoded before these keys existed, so
    // a config written for an older worker keeps its behaviour exactly.
    out.capsFlags = getCapsFlags(config, "caps_flags",
                                 MXL_FABRICS_IFACE_CAP_REMOTE_WRITE |
                                     MXL_FABRICS_IFACE_CAP_BLOCKING_OPERATIONS);
    out.maxMessageSize = getUint64(config, "max_message_size", 0);

    // 10 s to match the previously hardcoded value. A supervisor that wants a
    // paused session to sit quietly instead of cycling sets 0.
    out.idleTimeout = getTimeout(config, "idle_timeout_ms", 10000);

    // The connect loop previously had no timeout at all: an initiator pointed
    // at an unreachable target waited forever and only a signal broke it out,
    // which is a hang rather than a state anything can act on. The default is
    // generous enough that it only fires on a genuine failure, and a
    // supervisor that would rather wait can still set 0.
    out.connectTimeout = getTimeout(config, "connect_timeout_ms", 60000);

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

fabrics::InterfaceConfig Config::interfaceConfig() const noexcept {
    return {
        .provider = provider,
        .capsFlags = capsFlags,
        .maxMessageSize = maxMessageSize,
    };
}

} // namespace mxl::proxy
