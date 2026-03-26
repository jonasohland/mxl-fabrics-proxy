#pragma once

#include "config.hpp"
#include "fabrics.hpp"
#include "metrics.hpp"
#include "mxl.hpp"
#include "util.hpp"

namespace mxl::proxy {
class Initiator {
  public:
    static void run(Config config, utils::ExitSignal sig);

  private:
    explicit Initiator(Config config);

    void run(utils::ExitSignal sig);

    ::mxl::DiscreteFlowReader openReader();
    ::mxl::DiscreteFlowWriter
    openWriter(::mxl::DiscreteFlowReader const& reader);

    ::mxl::fabrics::DiscreteFlowInitiator
    createInitiator(::mxl::DiscreteFlowReader&);

    void connect(::mxl::fabrics::DiscreteFlowInitiator&, utils::ExitSignal sig);

    void transferGrains(::mxl::DiscreteFlowReader,
                        std::optional<::mxl::DiscreteFlowWriter>,
                        ::mxl::fabrics::DiscreteFlowInitiator,
                        utils::ExitSignal sig);

  private:
    [[nodiscard]] bool measurePreciseNetworkLatency() const noexcept;

    std::uint64_t _lastIndex = 0;

  private:
    ::mxl::Instance _mxl;
    ::mxl::fabrics::Instance _fabrics;
    Metrics _metrics;
    Config _config;
};
} // namespace mxl::proxy
