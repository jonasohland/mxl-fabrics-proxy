#include "util.hpp"

namespace utils {
ExitSignal::ExitSignal(volatile std::sig_atomic_t* sig) : _signal(sig) {
}

bool ExitSignal::shouldExit() const noexcept {
    return *_signal != 0;
}
} // namespace utils
