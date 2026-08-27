#include "output.hpp"

#include <cstdio>
#include <system_error>
#include <unistd.h>

namespace mxl::mock {

StdoutGuard::StdoutGuard()
    : _saved{-1} {
    std::fflush(stdout);
    _saved = ::dup(STDOUT_FILENO);
    if (_saved < 0) {
        throw std::system_error{errno, std::generic_category(), "dup"};
    }
    if (::dup2(STDERR_FILENO, STDOUT_FILENO) < 0) {
        ::close(_saved);
        _saved = -1;
        throw std::system_error{errno, std::generic_category(), "dup2"};
    }
}

StdoutGuard::~StdoutGuard() {
    restore();
}

void StdoutGuard::restore() noexcept {
    if (_saved < 0) {
        return;
    }
    std::fflush(stdout);
    ::dup2(_saved, STDOUT_FILENO);
    ::close(_saved);
    _saved = -1;
}

} // namespace mxl::mock
