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

    ::mxl::DiscreteFlowWriter
    openWriter(::mxl::DiscreteFlowReader const& reader);

    ::mxl::fabrics::DiscreteFlowInitiator
    createInitiator(::mxl::DiscreteFlowReader&);
    ::mxl::fabrics::ContinuousFlowInitiator
    createInitiator(::mxl::ContinuousFlowReader&);

    void connect(::mxl::fabrics::DiscreteFlowInitiator&, utils::ExitSignal sig);
    void connect(::mxl::fabrics::ContinuousFlowInitiator&,
                 utils::ExitSignal sig);

    // Owns the latency-measurement writer for the duration, opening it against
    // the first grain and dropping it whenever the source stalls: it may not
    // outlive the grains, or it strands the flow when the target that owns it
    // is withdrawn.
    void transferGrains(::mxl::DiscreteFlowReader,
                        ::mxl::fabrics::DiscreteFlowInitiator,
                        utils::ExitSignal sig);
    void transferSamples(::mxl::ContinuousFlowReader,
                         ::mxl::fabrics::ContinuousFlowInitiator,
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
