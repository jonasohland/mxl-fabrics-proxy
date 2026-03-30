#include "initiator.hpp"
#include "fabrics.hpp"
#include "rt.hpp"
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
      _metrics(config.metricsSocket, false) {
}

bool Initiator::measurePreciseNetworkLatency() const noexcept {
    return !_config.noNetworkLatencyMeasurement;
}

void Initiator::run(utils::ExitSignal sig) {
    std::optional<::mxl::DiscreteFlowWriter> writer = std::nullopt;
    auto reader = openReader();
    if (measurePreciseNetworkLatency()) {
        writer.emplace(openWriter(reader));
    }

    auto initiator = createInitiator(reader);
    auto targetInfo = ::mxl::fabrics::TargetInfo::parse(_config.targetInfo);
    initiator.addTarget(targetInfo);
    connect(initiator, sig);
    auto _ = ScopedRTScheduling{_config.schedPrio};
    transferGrains(std::move(reader), std::move(writer), std::move(initiator),
                   sig);
}

::mxl::DiscreteFlowReader Initiator::openReader() {
    auto flowReader = _mxl.openFlow(_config.flowId);
    if (!std::holds_alternative<DiscreteFlowReader>(flowReader)) {
        throw ::mxl::Exception{MXL_ERR_INVALID_ARG,
                               "request flow is not a discrete flow"};
    }

    return std::get<DiscreteFlowReader>(std::move(flowReader));
}

::mxl::DiscreteFlowWriter
Initiator::openWriter(::mxl::DiscreteFlowReader const& reader) {
    auto writer = _mxl.createFlow(reader.getFlowDefinition());
    if (!std::holds_alternative<DiscreteFlowWriter>(writer)) {
        throw ::mxl::Exception{MXL_ERR_INVALID_ARG,
                               "request flow is not a discrete flow"};
    }

    return std::get<DiscreteFlowWriter>(std::move(writer));
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

        if (initiator.makeProgress(std::chrono::milliseconds(500))) {
            continue;
        }

        spdlog::info("connected");
        return;
    }
}

void Initiator::transferGrains(::mxl::DiscreteFlowReader reader,
                               std::optional<::mxl::DiscreteFlowWriter> writer,
                               ::mxl::fabrics::DiscreteFlowInitiator initiator,
                               utils::ExitSignal sig) {
    std::uint64_t index = 0;
    auto lastSuccessfullGrainRead = std::chrono::steady_clock::now();
    for (;;) {
        try {
            if (sig.shouldExit()) {
                return;
            }

            auto grainAccess =
                reader.getGrain(index, std::chrono::milliseconds(100));
            lastSuccessfullGrainRead = std::chrono::steady_clock::now();
            auto rxTime = ::mxlGetTime();
            if (measurePreciseNetworkLatency()) {
                auto writeAccess = writer->openGrain(index);
                writeAccess.writeTxTimestamp(::mxlGetTime());
                writeAccess.cancel();
            }
            spdlog::debug("transmitting grain index={} fromSlice={} toSlice={}",
                          index, 0, grainAccess.validSlices());
            if (!initiator.transfer(index, 0, grainAccess.validSlices())) {
                spdlog::warn("grain at index={} discarded because initiator is "
                             "not ready",
                             index);
            }
            while (initiator.makeProgress(std::chrono::milliseconds(1000))) {
                spdlog::warn("grain still not transmitted after timeout");
                if (sig.shouldExit()) {
                    return;
                }
            }

            // Calculate the number of skipped grains
            std::uint64_t skipped = 0;
            if (_lastIndex < index) {
                if (_lastIndex != 0) {
                    std::uint64_t skipped = 0;
                    skipped = index - (_lastIndex + 1);
                }

                _lastIndex = index;
            }

            auto rate = reader.getRate();
            auto grainSize = grainAccess.size();

            // Calculate the source latency
            auto grainTime = ::mxlIndexToTimestamp(&rate, index);
            auto sourceLatency = std::uint64_t{0};
            if (rxTime > grainTime) {
                sourceLatency = rxTime - grainTime;
            }

            _metrics.observe(grainSize, grainSize + 4096, 1, skipped,
                             sourceLatency, 0);
            ++index;
        } catch (::mxl::Exception const& ex) {
            auto timeSinceLastGrainRead =
                std::chrono::steady_clock::now() - lastSuccessfullGrainRead;
            if (timeSinceLastGrainRead > std::chrono::seconds(10)) {
                throw Exception{MXL_ERR_TIMEOUT,
                                "timed out waiting for a grain to be published "
                                "to the flow"};
            }

            if (ex.isTooEarly() || ex.isTooLate()) {
                index = reader.getHeadIndex() + 1;
                continue;
            }

            throw ex;
        }
    }
}
} // namespace mxl::proxy
