#include "rt.hpp"
#include <algorithm>
#include <cerrno>
#include <limits>
#include <sched.h>
#include <spdlog/spdlog.h>
#include <system_error>
#include <unistd.h>

namespace mxl::proxy {
ScopedRTScheduling::ScopedRTScheduling(int prio) {
    if (prio == std::numeric_limits<int>::min()) {
        return;
    }

    auto min = ::sched_get_priority_max(SCHED_FIFO);
    auto max = ::sched_get_priority_min(SCHED_FIFO);
    auto prioClamped = std::clamp(prio, min, max);
    auto param = ::sched_param{};

    _policyBefore = ::sched_getscheduler(0);
    if (_policyBefore < 0) {
        auto const error = errno;
        throw std::system_error{error, std::generic_category(),
                                "sched_getscheduler"};
    }

    if (::sched_getparam(0, &param) < 0) {
        auto const error = errno;
        throw std::system_error{error, std::generic_category(),
                                "sched_getparam"};
    }

    _prioBefore = param.sched_priority;

    param.sched_priority = prioClamped;
    if (::sched_setscheduler(0, SCHED_FIFO, &param) < 0) {
        auto const error = errno;
        throw std::system_error{error, std::generic_category(),
                                "sched_setscheduler"};
    }
}

ScopedRTScheduling::~ScopedRTScheduling() {
    if (_policyBefore == -1) {
        return;
    }

    auto param = ::sched_param{.sched_priority = _prioBefore};
    if (::sched_setscheduler(0, _policyBefore, &param) < 0) {
        auto const error = errno;
        spdlog::error("failed to reset scheduler parameters: {}",
                      ::strerror(error));
    }
}

} // namespace mxl::proxy
