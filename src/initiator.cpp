#include "initiator.hpp"
#include "fabrics.hpp"
#include "util.hpp"
#include <chrono>
#include <mxl/time.h>
#include <spdlog/spdlog.h>

namespace mxl::proxy {
void Initiator::run(Config config, utils::ExitSignal sig) {
    Initiator{std::move(config)}.run(sig);
}

Initiator::Initiator(Config config)
    : _mxl(config.domain),
      _fabrics(_mxl),
      _config(config),
      _metrics(config.metricsSocket) {
}

void Initiator::run(utils::ExitSignal sig) {
    if (_config.provider == MXL_FABRICS_PROVIDER_EFA && _config.efaUseWait) {
        spdlog::info("using efa provider with blocking completion queue reads");
    }
    auto reader = openReader();
    auto initiator = createInitiator(reader);
    auto targetInfo = ::mxl::fabrics::TargetInfo::parse(_config.targetInfo);
    initiator.addTarget(targetInfo);
    connect(initiator, sig);
    transferGrains(std::move(reader), std::move(initiator), sig);
}

::mxl::DiscreteFlowReader Initiator::openReader() {
    auto flowReader = _mxl.openFlow(_config.flowId);
    if (!std::holds_alternative<DiscreteFlowReader>(flowReader)) {
        throw ::mxl::Exception{MXL_ERR_INVALID_ARG,
                               "request flow is not a discrete flow"};
    }

    return std::get<DiscreteFlowReader>(std::move(flowReader));
}

::mxl::fabrics::DiscreteFlowInitiator
Initiator::createInitiator(DiscreteFlowReader& reader) {
    return _fabrics.createInitiator(reader, {
                                                .node = _config.node,
                                                .service = _config.service,
                                                .provider = _config.provider,
                                            });
}

void Initiator::connect(::mxl::fabrics::DiscreteFlowInitiator& initiator,
                        utils::ExitSignal sig) {
    spdlog::info("waiting to connect...");
    for (;;) {
        if (sig.shouldExit()) {
            return;
        }

        if (_config.provider == MXL_FABRICS_PROVIDER_EFA &&
            !_config.efaUseWait) {
            if (initiator.makeProgressNonBlocking()) {
                continue;
            }
        } else {
            if (initiator.makeProgress(std::chrono::milliseconds(500))) {
                continue;
            }
        }

        spdlog::info("connected");
        return;
    }
}

void Initiator::transferGrains(::mxl::DiscreteFlowReader reader,
                               ::mxl::fabrics::DiscreteFlowInitiator initiator,
                               utils::ExitSignal sig) {
    std::uint64_t index = 0;
    for (;;) {
        try {
            if (sig.shouldExit()) {
                return;
            }

            auto grainAccess =
                reader.getGrain(index, std::chrono::milliseconds(100));
            auto inputTs = ::mxlGetTime();
            spdlog::debug("transmitting grain index={} fromSlice={} toSlice={}",
                          index, 0, grainAccess.validSlices());
            if (!initiator.transfer(index, 0, grainAccess.validSlices())) {
                spdlog::warn("grain at index={} discarded because initiator is "
                             "not ready",
                             index);
            }
            if (_config.provider == MXL_FABRICS_PROVIDER_EFA &&
                !_config.efaUseWait) {
                while (initiator.makeProgressNonBlocking()) {
                    if (sig.shouldExit()) {
                        return;
                    }
                }
            } else {
                while (
                    initiator.makeProgress(std::chrono::milliseconds(1000))) {
                    spdlog::warn("grain still not transmitted after timeout");
                    if (sig.shouldExit()) {
                        return;
                    }
                }
            }

            std::uint64_t skipped = 0;
            if (_lastIndex != 0) {
                skipped = index - (_lastIndex + 1);
            }

            _lastIndex = index;

            auto rate = reader.getRate();
            auto epochTs = ::mxlIndexToTimestamp(&rate, index);
            auto latency = inputTs - epochTs;
            auto grainSize = grainAccess.size();
            _metrics.observe(grainSize, grainSize + 4096, 1, skipped, latency);
            ++index;
        } catch (::mxl::Exception const& ex) {
            if (ex.isTooEarly() || ex.isTooLate()) {
                index = reader.getHeadIndex();
                continue;
            }

            throw ex;
        }
    }
}
} // namespace mxl::proxy
