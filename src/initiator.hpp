#pragma once

#include "config.hpp"
#include "fabrics.hpp"
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

    ::mxl::fabrics::DiscreteFlowInitiator
    createInitiator(::mxl::DiscreteFlowReader&);

    void connect(::mxl::fabrics::DiscreteFlowInitiator&, utils::ExitSignal sig);

    void transferGrains(::mxl::DiscreteFlowReader,
                        ::mxl::fabrics::DiscreteFlowInitiator,
                        utils::ExitSignal sig);

  private:
    ::mxl::Instance _mxl;
    ::mxl::fabrics::Instance _fabrics;
    Config _config;
};
} // namespace mxl::proxy
