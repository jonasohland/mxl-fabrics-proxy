#include "fabrics.hpp"
#include "deferred.hpp"
#include <mxl/fabrics.h>
#include <mxl/mxl.h>
#include <nlohmann/json.hpp>
#include <spdlog/spdlog.h>

namespace mxl::fabrics {

TargetInfo::TargetInfo(std::string id, ::mxlFabricsTargetInfo info)
    : _id(std::move(id)), _targetInfo(info) {
}

TargetInfo::TargetInfo(::mxlFabricsTargetInfo info) : _id(), _targetInfo(info) {
    auto parsed = nlohmann::json::parse(toString());
    if (auto it = parsed.find("id"); it != parsed.end() && it->is_string()) {
        _id = it->get<std::string>();
    } else {
        throw Exception{MXL_ERR_INVALID_ARG, "invalid target info"};
    }
}

TargetInfo::~TargetInfo() {
    if (_targetInfo) {
        ::mxlFabricsFreeTargetInfo(_targetInfo);
    }
}

TargetInfo::TargetInfo(TargetInfo&& other)
    : _id(std::move(other._id)), _targetInfo(nullptr) {
    std::swap(_targetInfo, other._targetInfo);
}

TargetInfo TargetInfo::parse(const std::string& s) {
    std::string id{};
    ::mxlFabricsTargetInfo targetInfo{};
    auto parsed = nlohmann::json::parse(s);
    if (auto it = parsed.find("id"); it != parsed.end() && it->is_string()) {
        id = it->get<std::string>();
    } else {
        throw Exception{MXL_ERR_INVALID_ARG, "invalid target info"};
    }
    mxl(::mxlFabricsTargetInfoFromString, "failed to parse target info",
        s.c_str(), &targetInfo);
    return {std::move(id), targetInfo};
}

std::string TargetInfo::id() const noexcept {
    return _id;
}

std::string TargetInfo::toString() const {
    std::string buffer{};
    std::size_t size = 0;
    mxl(::mxlFabricsTargetInfoToString, "failed to get target info string size",
        _targetInfo, nullptr, &size);
    buffer.resize(size);
    mxl(::mxlFabricsTargetInfoToString,
        "failed to convert target info to string", _targetInfo, buffer.data(),
        &size);
    return buffer;
}

::mxlFabricsTargetInfo TargetInfo::raw() const {
    return _targetInfo;
}

Instance::Instance(mxl::Instance instance)
    : _inner(std::make_shared<Inner>(std::move(instance))) {
}

Instance::Inner::Inner(mxl::Instance mxlInstance)
    : mxlInstance(mxlInstance), instance(nullptr) {
    mxl(::mxlFabricsCreateInstance, "failed to create fabrics instance",
        mxlInstance.raw(), &instance);
}

Instance::Inner::~Inner() {
    ::mxlFabricsDestroyInstance(instance);
}

std::pair<DiscreteFlowTarget, TargetInfo>
Instance::createTarget(DiscreteFlowWriter& writer, TargetConfig targetConfig) {
    ::mxlFabricsTarget target{};
    ::mxlFabricsRegions regions{};

    // create a regions object from the writer
    mxl(::mxlFabricsRegionsForFlowWriter,
        "failed to get memory regions for flow writer", writer.raw(), &regions);
    auto _d0 = defer([&]() { ::mxlFabricsRegionsFree(regions); });

    ::mxlFabricsTargetConfig config = {
        .endpointAddress =
            ::mxlFabricsEndpointAddress{
                .node = targetConfig.node.c_str(),
                .service = targetConfig.service.c_str(),
            },
        .provider = targetConfig.provider,
        .regions = regions,
        .deviceSupport = false,
    };

    // create the target
    mxl(::mxlFabricsCreateTarget, "failed to create target", _inner->instance,
        &target);

    auto discreteFlowTarget = DiscreteFlowTarget{*this, target};
    auto targetInfo = discreteFlowTarget.setup(config);
    return {std::move(discreteFlowTarget), std::move(targetInfo)};
}

DiscreteFlowInitiator
Instance::createInitiator(DiscreteFlowReader& reader,
                          InitiatorConfig initiatorConfig) {
    ::mxlFabricsInitiator initiator{};
    ::mxlFabricsRegions regions{};

    mxl(::mxlFabricsRegionsForFlowReader,
        "failed to get memory regions for flow reader", reader.raw(), &regions);
    auto _d0 = defer([&]() { ::mxlFabricsRegionsFree(regions); });

    mxl(::mxlFabricsCreateInitiator, "failed to create initiator",
        _inner->instance, &initiator);
    auto discreteFlowInitiator = DiscreteFlowInitiator{*this, initiator};

    ::mxlFabricsInitiatorConfig config = {
        .endpointAddress =
            ::mxlFabricsEndpointAddress{
                .node = initiatorConfig.node.c_str(),
                .service = initiatorConfig.service.c_str(),
            },

        .provider = initiatorConfig.provider,
        .regions = regions,
        .deviceSupport = false,
    };

    discreteFlowInitiator.setup(config);
    return discreteFlowInitiator;
}

void Instance::destroy(::mxlFabricsInitiator initiator) {
    try {
        mxl(::mxlFabricsDestroyInitiator, "", _inner->instance, initiator);
    } catch (std::exception const& ex) {
        spdlog::error("failed to destroy initiator: {}", ex.what());
    }
}

void Instance::destroy(::mxlFabricsTarget target) {
    try {
        mxl(::mxlFabricsDestroyTarget, "", _inner->instance, target);
    } catch (std::exception const& ex) {
        spdlog::error("failed to destroy target: {}", ex.what());
    }
}

DiscreteFlowTarget::DiscreteFlowTarget(Instance instance,
                                       ::mxlFabricsTarget target)
    : _instance(std::move(instance)), _target(target) {
}

DiscreteFlowTarget::DiscreteFlowTarget(DiscreteFlowTarget&& target)
    : _instance(std::move(target._instance)), _target(nullptr) {
    std::swap(_target, target._target);
}

DiscreteFlowTarget::~DiscreteFlowTarget() {
    if (_target) {
        _instance.destroy(_target);
        _target = nullptr;
    }
}

TargetInfo DiscreteFlowTarget::setup(::mxlFabricsTargetConfig const& config) {
    ::mxlFabricsTargetInfo targetInfo{};
    mxl(::mxlFabricsTargetSetup, "failed to setup target", _target, &config,
        &targetInfo);
    return {targetInfo};
}

std::optional<std::uint64_t>
DiscreteFlowTarget::readGrain(std::chrono::milliseconds timeout) {
    std::uint64_t index = 0;
    try {
        mxl(::mxlFabricsTargetReadGrain, "failed to read grain", _target,
            timeout.count(), &index);
    } catch (Exception& ex) {
        if (ex.isNotReady() || ex.isTimeout()) {
            return {};
        }

        throw ex;
    }

    return {index};
}

std::optional<std::uint64_t> DiscreteFlowTarget::readGrainNonBlocking() {
    std::uint64_t index = 0;
    try {
        mxl(::mxlFabricsTargetReadGrainNonBlocking, "failed to read grain",
            _target, &index);
    } catch (Exception& ex) {
        if (ex.isNotReady()) {
            return {};
        }

        throw ex;
    }

    return {index};
}

DiscreteFlowInitiator::DiscreteFlowInitiator(Instance instance,
                                             ::mxlFabricsInitiator initiator)
    : _instance(std::move(instance)), _initiator(initiator) {
}

DiscreteFlowInitiator::DiscreteFlowInitiator(DiscreteFlowInitiator&& initiator)
    : _instance(std::move(initiator._instance)), _initiator(nullptr) {
    std::swap(_initiator, initiator._initiator);
}

DiscreteFlowInitiator::~DiscreteFlowInitiator() {
    if (_initiator) {
        _instance.destroy(_initiator);
        _initiator = nullptr;
    }
}

void DiscreteFlowInitiator::setup(::mxlFabricsInitiatorConfig const& config) {
    mxl(::mxlFabricsInitiatorSetup, "failed to setup initiator", _initiator,
        &config);
}

void DiscreteFlowInitiator::addTarget(TargetInfo const& targetInfo) {
    mxl(::mxlFabricsInitiatorAddTarget, "failed to add target", _initiator,
        targetInfo.raw());
}

void DiscreteFlowInitiator::removeTarget(TargetInfo const& targetInfo) {
    mxl(::mxlFabricsInitiatorRemoveTarget, "failed to remove target",
        _initiator, targetInfo.raw());
}

bool DiscreteFlowInitiator::transfer(std::uint64_t index,
                                     std::uint16_t fromSlice,
                                     std::uint16_t toSlice) {
    try {
        mxl(::mxlFabricsInitiatorTransferGrain, "failed to transfer grain",
            _initiator, index, fromSlice, toSlice);
    } catch (::mxl::Exception const& ex) {
        if (ex.isNotReady()) {
            return false;
        }

        throw ex;
    }

    return true;
}

bool DiscreteFlowInitiator::makeProgress(std::chrono::milliseconds timeout) {
    try {
        mxl(::mxlFabricsInitiatorMakeProgressBlocking, "make progress error",
            _initiator, timeout.count());
    } catch (mxl::Exception& ex) {
        if (ex.status() == MXL_ERR_NOT_READY ||
            ex.status() == MXL_ERR_TIMEOUT) {
            return true;
        }

        throw ex;
    }

    return false;
}

bool DiscreteFlowInitiator::makeProgressNonBlocking() {
    try {
        mxl(::mxlFabricsInitiatorMakeProgressNonBlocking, "make progress error",
            _initiator);
    } catch (Exception& ex) {
        if (ex.status() == MXL_ERR_NOT_READY) {
            return true;
        }

        throw ex;
    }

    return false;
}

} // namespace mxl::fabrics
