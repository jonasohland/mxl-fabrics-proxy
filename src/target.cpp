#include "target.hpp"
#include "rt.hpp"
#include <fstream>
#include <mxl/time.h>
#include <spdlog/spdlog.h>
#include <system_error>

namespace {
void writeFile(std::filesystem::path const& path, std::string const& contents) {
    std::ofstream ofs{path, std::ios::binary | std::ios::out | std::ios::trunc};
    if (!ofs.is_open()) {
        throw std::system_error{errno, std::generic_category(),
                                std::format("open {}", path.string())};
    }
    ofs << contents;
    if (ofs.bad()) {
        throw std::system_error{errno, std::generic_category(),
                                std::format("write to {}", path.string())};
    }
}
} // namespace

namespace mxl::proxy {
void Target::run(Config config, utils::ExitSignal sig) {
    Target{std::move(config)}.run(sig);
}

Target::Target(Config config)
    : _mxl(config.domain),
      _fabrics(_mxl),
      _config(config),
      _metrics(config.metricsSocket, !config.noNetworkLatencyMeasurement) {
}

bool Target::measurePreciseNetworkLatency() const noexcept {
    return !_config.noNetworkLatencyMeasurement;
}

void Target::run(utils::ExitSignal sig) {
    auto writerVariant = createWriter();
    if (std::holds_alternative<::mxl::DiscreteFlowWriter>(writerVariant)) {
        auto writer =
            std::get<::mxl::DiscreteFlowWriter>(std::move(writerVariant));
        auto [target, targetInfo] = createTarget(writer);
        writeFile(_config.targetInfo, targetInfo.toString());
        auto _ = ScopedRTScheduling{_config.schedPrio};
        transferGrains(std::move(writer), std::move(target), sig);
    } else {
        auto writer =
            std::get<::mxl::ContinuousFlowWriter>(std::move(writerVariant));
        auto [target, targetInfo] = createTarget(writer);
        writeFile(_config.targetInfo, targetInfo.toString());
        auto _ = ScopedRTScheduling{_config.schedPrio};
        transferSamples(std::move(writer), std::move(target), sig);
    }
}

std::variant<::mxl::DiscreteFlowWriter, ::mxl::ContinuousFlowWriter>
Target::createWriter() const {
    return _mxl.createFlow(_config.flowDef);
}

std::pair<::mxl::fabrics::DiscreteFlowTarget, ::mxl::fabrics::TargetInfo>
Target::createTarget(::mxl::DiscreteFlowWriter& writer) {
    return _fabrics.createTarget(writer, {.node = _config.node,
                                          .service = _config.service,
                                          .provider = _config.provider});
}

std::pair<::mxl::fabrics::ContinuousFlowTarget, ::mxl::fabrics::TargetInfo>
Target::createTarget(::mxl::ContinuousFlowWriter& writer) {
    return _fabrics.createTarget(writer, {.node = _config.node,
                                          .service = _config.service,
                                          .provider = _config.provider});
}

void Target::transferGrains(::mxl::DiscreteFlowWriter writer,
                            ::mxl::fabrics::DiscreteFlowTarget target,
                            utils::ExitSignal sig) {
    for (;;) {
        auto txTime = std::uint64_t{0};
        auto sourceLatency = std::uint64_t{0};
        auto networkLatency = std::uint64_t{0};
        auto grainSize = std::uint64_t{0};

        // start reading the next grain
        auto index = readNextGrain(target, sig);
        auto rxTime = ::mxlGetTime();
        if (sig.shouldExit()) {
            return;
        }
        {
            auto access = writer.openGrain(index);
            grainSize = access.size();
            spdlog::debug(
                "comitting grain validSlices={} totalSlices={} index={}",
                access.validSlices(), access.totalSlices(), index);

            // read the tx timestamp from the grain header if enabled
            if (measurePreciseNetworkLatency()) {
                txTime = access.readTxTimestamp();
            }
            // committed when access object it dropped
        }

        std::uint64_t skipped = 0;
        if (_lastIndex < index) {
            if (_lastIndex != 0) {
                skipped = index - (_lastIndex + 1);
            }

            _lastIndex = index;
        }

        // calculate the source latency
        auto rate = writer.getRate();
        auto grainTime = ::mxlIndexToTimestamp(&rate, index);
        if (rxTime > grainTime) {
            sourceLatency = rxTime - grainTime;
        }

        // calculate the network latency
        if (measurePreciseNetworkLatency() && (rxTime > txTime)) {
            networkLatency = rxTime - txTime;
        }

        _metrics.observe(grainSize, grainSize + 4096, 1, skipped, sourceLatency,
                         networkLatency);
    }
}

std::uint64_t Target::readNextGrain(::mxl::fabrics::DiscreteFlowTarget& target,
                                    utils::ExitSignal sig) {
    std::chrono::system_clock::time_point lastRead =
        std::chrono::system_clock::now();
    for (;;) {
        std::optional<std::uint64_t> res{std::nullopt};
        if (sig.shouldExit()) {
            return 0;
        }
        res = target.readGrain(std::chrono::milliseconds(500));
        if (!res) {
            auto waitTime = std::chrono::system_clock::now() - lastRead;
            if (waitTime > std::chrono::seconds(10)) {
                throw ::mxl::Exception{MXL_ERR_TIMEOUT,
                                       "timed out waiting for a grain"};
            }

            continue;
        }

        return *res;
    }
}

void Target::transferSamples(::mxl::ContinuousFlowWriter writer,
                             ::mxl::fabrics::ContinuousFlowTarget target,
                             utils::ExitSignal sig) {
    for (;;) {
        auto [headIndex, count] = readNextSamples(target, sig);
        auto rxTime = ::mxlGetTime();
        if (sig.shouldExit()) {
            return;
        }
        {
            auto access = writer.openSamples(headIndex, count);
            spdlog::debug("committing samples headIndex={} count={}", headIndex,
                          count);
            // RAII: access auto-commits on destruction
        }

        std::uint64_t skipped = 0;
        if (_lastIndex != 0 && headIndex > _lastIndex) {
            skipped = headIndex - _lastIndex;
        }
        _lastIndex = headIndex + count;

        auto rate = writer.getRate();
        auto grainTime = ::mxlIndexToTimestamp(&rate, headIndex);
        auto sourceLatency = std::uint64_t{0};
        if (rxTime > grainTime) {
            sourceLatency = rxTime - grainTime;
        }

        _metrics.observe(count, count, 1, skipped, sourceLatency, 0);
    }
}

std::pair<std::uint64_t, std::size_t>
Target::readNextSamples(::mxl::fabrics::ContinuousFlowTarget& target,
                        utils::ExitSignal sig) {
    auto lastRead = std::chrono::system_clock::now();
    for (;;) {
        if (sig.shouldExit()) {
            return {0, 0};
        }
        auto result = target.readSamples(std::chrono::milliseconds(500));
        if (!result) {
            auto waitTime = std::chrono::system_clock::now() - lastRead;
            if (waitTime > std::chrono::seconds(10)) {
                throw ::mxl::Exception{MXL_ERR_TIMEOUT,
                                       "timed out waiting for samples"};
            }
            continue;
        }
        return *result;
    }
}

} // namespace mxl::proxy
