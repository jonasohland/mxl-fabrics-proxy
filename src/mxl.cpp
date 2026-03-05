#include "mxl.hpp"
#include <format>
#include <mxl/flow.h>
#include <mxl/flowinfo.h>
#include <mxl/mxl.h>
#include <mxl/time.h>
#include <stdexcept>

namespace mxl {

Exception::Exception(::mxlStatus status, char const* msg)
    : _status(status),
      _msg(msg) {
}

Exception::Exception(::mxlStatus status, std::string msg)
    : _status(status),
      _msg(""),
      _formatted(std::move(msg)) {
}

char const* Exception::what() const noexcept {
    ensureFormatted();
    return _formatted.c_str();
}

::mxlStatus Exception::status() const noexcept {
    return _status;
}

bool Exception::isOutOfRange() const noexcept {
    return isTooEarly() || isTooLate();
}

bool Exception::isTooEarly() const noexcept {
    return _status == MXL_ERR_OUT_OF_RANGE_TOO_EARLY;
}

bool Exception::isTooLate() const noexcept {
    return _status == MXL_ERR_OUT_OF_RANGE_TOO_LATE;
}

bool Exception::isFlowInvalid() const noexcept {
    return _status == MXL_ERR_FLOW_INVALID;
}

bool Exception::isInterrupted() const noexcept {
    return _status == MXL_ERR_INTERRUPTED;
}

bool Exception::isNotReady() const noexcept {
    return _status == MXL_ERR_NOT_READY;
}

bool Exception::isTimeout() const noexcept {
    return _status == MXL_ERR_TIMEOUT;
}

void Exception::ensureFormatted() const noexcept {
    if (!_formatted.empty()) {
        return;
    }
    try {
        _formatted = std::format("{}: {}", mxlStatusName(_status), _msg);
    } catch (...) {
        _formatted = _msg;
    }
}

std::string_view mxlStatusName(::mxlStatus status) {
    switch (status) {
    case MXL_STATUS_OK:
        return "ok";
    case MXL_ERR_UNKNOWN:
        return "unknown error";
    case MXL_ERR_FLOW_NOT_FOUND:
        return "flow not found";
    case MXL_ERR_OUT_OF_RANGE_TOO_LATE:
        return "too late";
    case MXL_ERR_OUT_OF_RANGE_TOO_EARLY:
        return "too early";
    case MXL_ERR_INVALID_FLOW_READER:
        return "invalid flow reader";
    case MXL_ERR_INVALID_FLOW_WRITER:
        return "invalid flow writer";
    case MXL_ERR_TIMEOUT:
        return "timeout";
    case MXL_ERR_INVALID_ARG:
        return "invalid argument";
    case MXL_ERR_CONFLICT:
        return "conflict";
    case MXL_ERR_PERMISSION_DENIED:
        return "permission denied";
    case MXL_ERR_FLOW_INVALID:
        return "flow invalid";
    case MXL_ERR_STRLEN:
        return "invalid string length";
    case MXL_ERR_INTERRUPTED:
        return "interrupted";
    case MXL_ERR_NO_FABRIC:
        return "no fabric available";
    case MXL_ERR_INVALID_STATE:
        return "invalid state";
    case MXL_ERR_INTERNAL:
        return "internal error";
    case MXL_ERR_NOT_READY:
        return "not ready";
    case MXL_ERR_NOT_FOUND:
        return "not found";
    case MXL_ERR_EXISTS:
        return "exists";
    }
    return "unknown";
}

Instance::Inner::Inner(std::string const& domainPath)
    : instance(::mxlCreateInstance(domainPath.c_str(), nullptr)) {
    if (instance == nullptr) {
        throw std::runtime_error("failed to create mxl instance");
    }
}

Instance::Inner::~Inner() {
    ::mxlDestroyInstance(instance);
}

Instance::Instance(std::string const& domainPath)
    : _inner(std::make_shared<Inner>(domainPath)) {
}

void Instance::destroy(::mxlFlowWriter writer) const {
    mxl(::mxlReleaseFlowWriter, "failed to release flow reader",
        _inner->instance, writer);
}
void Instance::destroy(::mxlFlowReader reader) const {
    mxl(::mxlReleaseFlowReader, "failed to release flow reader",
        _inner->instance, reader);
}

std::variant<DiscreteFlowReader, ContinuousFlowReader>
Instance::openFlow(std::string const& id) const {
    auto reader = ::mxlFlowReader{};
    auto info = ::mxlFlowConfigInfo{};
    mxl(::mxlCreateFlowReader, "failed to create flow reader", _inner->instance,
        id.c_str(), nullptr, &reader);
    mxl(mxlFlowReaderGetConfigInfo, "failed to get config info from reader",
        reader, &info);
    switch (info.common.format) {
    case MXL_DATA_FORMAT_AUDIO:
        return ContinuousFlowReader{reader, info, *this};
    case MXL_DATA_FORMAT_VIDEO:
        return DiscreteFlowReader{reader, info, *this};
    case MXL_DATA_FORMAT_DATA:
        return DiscreteFlowReader{reader, info, *this};
    default:
        throw std::runtime_error{"invalid data format"};
    }
}

std::variant<DiscreteFlowWriter, ContinuousFlowWriter>
Instance::createFlow(std::string const& flowDef) const {
    auto writer = ::mxlFlowWriter{};
    auto info = ::mxlFlowConfigInfo{};
    auto created = false;
    mxl(::mxlCreateFlowWriter, "failed to create flow writer", _inner->instance,
        flowDef.c_str(), "", &writer, &info, &created);
    return DiscreteFlowWriter{writer, info, *this};
}

::mxlInstance Instance::raw() noexcept {
    return _inner->instance;
}

/** --------------- Discrete Flow Reader --------------- */

DiscreteFlowReader::DiscreteFlowReader(::mxlFlowReader reader,
                                       ::mxlFlowConfigInfo info,
                                       Instance instance)
    : _reader(reader),
      _configInfo(info),
      _instance(std::move(instance)) {
}

DiscreteFlowReader::DiscreteFlowReader(DiscreteFlowReader&& other)
    : _reader(nullptr),
      _configInfo(other._configInfo),
      _instance(std::move(other._instance)) {
    std::swap(_reader, other._reader);
}

DiscreteFlowReader::~DiscreteFlowReader() {
    if (_reader) {
        _instance.destroy(_reader);
    }
}

::mxlFlowReader DiscreteFlowReader::raw() {
    return _reader;
}

std::uint64_t DiscreteFlowReader::getHeadIndex() const {
    ::mxlFlowRuntimeInfo info{};
    mxl(::mxlFlowReaderGetRuntimeInfo, "failed to get flow info", _reader,
        &info);
    return info.headIndex;
}

::mxlRational DiscreteFlowReader::getRate() const {
    return _configInfo.common.grainRate;
}

DiscreteFlowReader::Access
DiscreteFlowReader::getGrain(std::uint64_t index,
                             std::chrono::nanoseconds timeout) {
    ::mxlGrainInfo info{};
    std::uint8_t* payload = nullptr;
    mxl(::mxlFlowReaderGetGrain, "failed to get grain", _reader, index,
        timeout.count(), &info, &payload);
    return {info, payload};
}

DiscreteFlowReader::Access
DiscreteFlowReader::getGrainNonBlocking(std::uint64_t index) {
    ::mxlGrainInfo info{};
    std::uint8_t* payload = nullptr;
    mxl(::mxlFlowReaderGetGrainNonBlocking, "failed to get grain", _reader,
        index, &info, &payload);
    return {info, payload};
}

DiscreteFlowReader::Access
DiscreteFlowReader::getGrainSlices(std::uint64_t index,
                                   std::uint16_t minValidSlices,
                                   std::chrono::nanoseconds timeout) {
    ::mxlGrainInfo info{};
    std::uint8_t* payload = nullptr;
    mxl(::mxlFlowReaderGetGrainSlice, "failed to get grain", _reader, index,
        minValidSlices, timeout.count(), &info, &payload);
    return {info, payload};
}

DiscreteFlowReader::Access
DiscreteFlowReader::getGrainSlicesNonBlocking(std::uint64_t index,
                                              std::uint16_t minValidSlices) {
    ::mxlGrainInfo info{};
    std::uint8_t* payload = nullptr;
    mxl(::mxlFlowReaderGetGrainSliceNonBlocking, "failed to get grain", _reader,
        index, minValidSlices, &info, &payload);
    return {info, payload};
}

DiscreteFlowReader::Access::Access(::mxlGrainInfo info,
                                   const std::uint8_t* payload)
    : _info(info),
      _payload(payload) {
}

std::size_t DiscreteFlowReader::Access::Access::size() const noexcept {
    return _info.grainSize;
}

std::uint8_t const* DiscreteFlowReader::Access::Access::data() const noexcept {
    return _payload;
}

std::uint16_t DiscreteFlowReader::Access::totalSlices() const noexcept {
    return _info.totalSlices;
}

std::uint16_t DiscreteFlowReader::Access::validSlices() const noexcept {
    return _info.validSlices;
}

/** --------------- Continuous Flow Reader --------------- */

ContinuousFlowReader::ContinuousFlowReader(::mxlFlowReader reader,
                                           ::mxlFlowConfigInfo info,
                                           Instance instance)
    : _reader(reader),
      _configInfo(info),
      _instance(std::move(instance)) {
}

ContinuousFlowReader::~ContinuousFlowReader() {
    _instance.destroy(_reader);
}

/// TODO implement

/** --------------- Discrete Flow Writer --------------- */

DiscreteFlowWriter::DiscreteFlowWriter(::mxlFlowWriter writer,
                                       ::mxlFlowConfigInfo info,
                                       Instance instance)
    : _writer(writer),
      _info(info),
      _instance(std::move(instance)) {
}

DiscreteFlowWriter::DiscreteFlowWriter(DiscreteFlowWriter&& other)
    : _writer(nullptr),
      _info(other._info),
      _instance(std::move(other._instance)) {
    std::swap(_writer, other._writer);
}

DiscreteFlowWriter::~DiscreteFlowWriter() {
    if (_writer) {
        _instance.destroy(_writer);
    }
}

::mxlFlowWriter DiscreteFlowWriter::raw() {
    return _writer;
}

DiscreteFlowWriter::Access DiscreteFlowWriter::openGrain(std::uint64_t index) {
    ::mxlGrainInfo grainInfo{};
    std::uint8_t* payload = nullptr;
    mxl(::mxlFlowWriterOpenGrain, "failed to open grain", _writer, index,
        &grainInfo, &payload);
    return Access{*this, grainInfo, payload};
}

::mxlRational DiscreteFlowWriter::getRate() const noexcept {
    return _info.common.grainRate;
}

void DiscreteFlowWriter::commit(::mxlGrainInfo const& info) {
    mxl(::mxlFlowWriterCommitGrain, "failed to commit grain", _writer, &info);
}

void DiscreteFlowWriter::cancel() {
    mxl(::mxlFlowWriterCancelGrain, "failed to cancel grain write", _writer);
}

DiscreteFlowWriter::Access::Access(DiscreteFlowWriter& writer,
                                   ::mxlGrainInfo info, std::uint8_t* payload)
    : _writer(writer),
      _info(info),
      _payload(payload) {
}

DiscreteFlowWriter::Access::~Access() noexcept(false) {
    if (_payload == nullptr) {
        return;
    }
    _writer.commit(_info);
}

void DiscreteFlowWriter::Access::cancel() {
    _payload = nullptr;
    _writer.cancel();
}

std::uint64_t DiscreteFlowWriter::Access::index() const noexcept {
    return _info.index;
}

std::size_t DiscreteFlowWriter::Access::size() const noexcept {
    return _info.grainSize;
}

std::uint8_t const* DiscreteFlowWriter::Access::data() const noexcept {
    return _payload;
}

std::uint8_t* DiscreteFlowWriter::Access::data() noexcept {
    return _payload;
}

std::uint16_t DiscreteFlowWriter::Access::totalSlices() const noexcept {
    return _info.totalSlices;
}

std::uint16_t DiscreteFlowWriter::Access::validSlices() const noexcept {
    return _info.validSlices;
}

void DiscreteFlowWriter::Access::validSlices(
    std::uint16_t validSlices) noexcept {
    _info.validSlices = validSlices;
}

} // namespace mxl
