#include "target.hpp"
#include <fstream>
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
    spdlog::info("worker running as target");
    Target{std::move(config)}.run(sig);
}

Target::Target(Config config)
    : _mxl(config.domain), _fabrics(_mxl), _config(config) {
}

void Target::run(utils::ExitSignal sig) {
    auto writer = createWriter();
    auto [target, targetInfo] = createTarget(writer);
    writeFile(_config.targetInfo, targetInfo.toString());
    transferGrains(std::move(writer), std::move(target), sig);
}

::mxl::DiscreteFlowWriter Target::createWriter() const {
    auto writer = _mxl.createFlow(_config.flowDef);
    if (!std::holds_alternative<DiscreteFlowWriter>(writer)) {
        throw ::mxl::Exception{MXL_ERR_INVALID_ARG,
                               "flow is not a discrete flow"};
    }

    return std::get<DiscreteFlowWriter>(std::move(writer));
}

std::pair<::mxl::fabrics::DiscreteFlowTarget, ::mxl::fabrics::TargetInfo>
Target::createTarget(::mxl::DiscreteFlowWriter& writer) {
    return _fabrics.createTarget(writer, {.node = _config.node,
                                          .service = _config.service,
                                          .provider = _config.provider});
}

void Target::transferGrains(::mxl::DiscreteFlowWriter writer,
                            ::mxl::fabrics::DiscreteFlowTarget target,
                            utils::ExitSignal sig) {
    for (;;) {
        auto index = readNextGrain(target, sig);
        if (sig.shouldExit()) {
            return;
        }
        {
            auto access = writer.openGrain(index);
            spdlog::debug(
                "comitting grain validSlices={} totalSlices={} index={}",
                access.validSlices(), access.totalSlices(), index);
            // committed when access object it dropped
        }
    }
}

std::uint64_t Target::readNextGrain(::mxl::fabrics::DiscreteFlowTarget& target,
                                    utils::ExitSignal sig) {
    for (;;) {
        std::optional<std::uint64_t> res{std::nullopt};
        if (sig.shouldExit()) {
            return 0;
        }
        if (_config.provider == MXL_FABRICS_PROVIDER_EFA) {
            res = target.readGrainNonBlocking();
        } else {
            res = target.readGrain(std::chrono::milliseconds(500));
        }
        if (!res) {
            continue;
        }

        return *res;
    }
}

} // namespace mxl::proxy
