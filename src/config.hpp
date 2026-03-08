#pragma once

#include <mxl/fabrics.h>
#include <nlohmann/json.hpp>
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
    bool efaUseWait;

    [[nodiscard]] bool isInitiator() const noexcept;
    [[nodiscard]] bool isTarget() const noexcept;

  private:
    static Config read(nlohmann::json config);
};

} // namespace mxl::proxy
