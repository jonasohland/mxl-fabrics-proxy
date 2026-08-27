#pragma once

#include "fabrics.hpp"
#include <chrono>
#include <cstdint>
#include <mxl/fabrics.h>
#include <nlohmann/json.hpp>
#include <optional>
#include <string>

namespace mxl::proxy {

struct Config {
    template <typename Input> static Config parse(Input&& in) {
        return read(nlohmann::json::parse(std::forward<Input>(in)));
    }

    bool target;
    std::string metricsSocket;
    std::string node;
    std::string service;
    std::string targetInfo;
    std::string flowDef;
    std::string flowId;
    std::string domain;
    std::string providerStr;
    ::mxlFabricsProvider provider;

    /// Negotiated interface capabilities. The fabrics library performs no
    /// capability negotiation of its own and requires both ends of a session to
    /// be configured identically, so these are decided out of band and handed
    /// to both workers. Absent from the config = the historical default of
    /// REMOTE_WRITE|BLOCKING_OPERATIONS, which keeps older configs working.
    std::uint64_t capsFlags;

    /// Maximum message size for the negotiated interface, in bytes. 0 = leave
    /// it to the library, which is what every config did before this key
    /// existed.
    std::uint64_t maxMessageSize;

    bool noNetworkLatencyMeasurement;
    int schedPrio;

    /// How long a role waits without receiving (target) or reading (initiator)
    /// a grain before it terminates. std::nullopt = wait indefinitely.
    ///
    /// Indefinite is what makes a paused session a steady state rather than a
    /// restart loop: a source with no producer is an ordinary thing to ask to
    /// replicate, and every target restart invalidates the target info, so a
    /// self-terminating idle worker costs a full control-plane round trip per
    /// cycle, per flow, forever.
    std::optional<std::chrono::milliseconds> idleTimeout;

    /// How long the initiator waits for its target to become reachable before
    /// giving up. std::nullopt = wait indefinitely, which was the only
    /// behaviour before this key existed.
    std::optional<std::chrono::milliseconds> connectTimeout;

    [[nodiscard]] bool isInitiator() const noexcept;
    [[nodiscard]] bool isTarget() const noexcept;

    /// The interface configuration to hand to the library, identical on both
    /// ends of a session by construction.
    [[nodiscard]] fabrics::InterfaceConfig interfaceConfig() const noexcept;

  private:
    static Config read(nlohmann::json config);
};

} // namespace mxl::proxy
