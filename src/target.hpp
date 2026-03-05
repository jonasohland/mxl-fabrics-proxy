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
    ::mxl::DiscreteFlowWriter createWriter() const;

    std::pair<::mxl::fabrics::DiscreteFlowTarget, ::mxl::fabrics::TargetInfo>
    createTarget(DiscreteFlowWriter&);

    void transferGrains(::mxl::DiscreteFlowWriter,
                        ::mxl::fabrics::DiscreteFlowTarget, utils::ExitSignal);

    std::uint64_t readNextGrain(::mxl::fabrics::DiscreteFlowTarget&,
                                utils::ExitSignal);

  private:
    ::mxl::Instance _mxl;
    ::mxl::fabrics::Instance _fabrics;
    Metrics _metrics;
    Config _config;
};
} // namespace mxl::proxy
