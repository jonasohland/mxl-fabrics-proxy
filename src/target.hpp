#pragma once

#include "config.hpp"
#include "fabrics.hpp"
#include "metrics.hpp"
#include "mxl.hpp"
#include "util.hpp"

namespace mxl::proxy {
class Target {
  public:
    static void run(Config config, utils::ExitSignal sig);

  private:
    explicit Target(Config config);

    void run(utils::ExitSignal sig);

    [[nodiscard]]
    std::variant<::mxl::DiscreteFlowWriter, ::mxl::ContinuousFlowWriter>
    createWriter() const;

    std::pair<::mxl::fabrics::DiscreteFlowTarget, ::mxl::fabrics::TargetInfo>
    createTarget(DiscreteFlowWriter&);
    std::pair<::mxl::fabrics::ContinuousFlowTarget, ::mxl::fabrics::TargetInfo>
    createTarget(::mxl::ContinuousFlowWriter&);

    void transferGrains(::mxl::DiscreteFlowWriter,
                        ::mxl::fabrics::DiscreteFlowTarget, utils::ExitSignal);
    void transferSamples(::mxl::ContinuousFlowWriter,
                         ::mxl::fabrics::ContinuousFlowTarget, utils::ExitSignal);

    std::uint64_t readNextGrain(::mxl::fabrics::DiscreteFlowTarget&,
                                utils::ExitSignal);
    std::pair<std::uint64_t, std::size_t>
    readNextSamples(::mxl::fabrics::ContinuousFlowTarget&, utils::ExitSignal);

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
