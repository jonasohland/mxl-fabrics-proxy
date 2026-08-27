#pragma once

namespace mxl::mock {

/// Points stdout at stderr for as long as it is alive.
///
/// The mock tools print a machine-readable summary on stdout and everything
/// else on stderr, so that a test harness can read one without disentangling
/// it from the other. That needs enforcing rather than merely intending,
/// because the mxl library installs a spdlog logger writing to **stdout**
/// the first time an instance is created, and libfabric's diagnostics are
/// routed into the same logger. Neither is under this program's control:
/// the library's logging setup runs behind a std::once_flag and replaces
/// whatever default logger was there.
///
/// So the same trick the worker's --interfaces probe uses (src/main.cpp):
/// redirect for the duration, [restore] before printing the data.
class StdoutGuard {
  public:
    StdoutGuard();
    ~StdoutGuard();

    StdoutGuard(StdoutGuard const&) = delete;
    StdoutGuard(StdoutGuard&&) = delete;
    StdoutGuard& operator=(StdoutGuard const&) = delete;
    StdoutGuard& operator=(StdoutGuard&&) = delete;

    /// Puts the real stdout back. Idempotent.
    void restore() noexcept;

  private:
    int _saved;
};

} // namespace mxl::mock
