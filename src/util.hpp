#pragma once

#include <csignal>
namespace utils {

class ExitSignal {
  public:
    explicit ExitSignal(volatile std::sig_atomic_t*);
    [[nodiscard]] bool shouldExit() const noexcept;

  private:
    volatile std::sig_atomic_t* _signal;
};

} // namespace utils
