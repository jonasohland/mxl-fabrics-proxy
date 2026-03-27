#pragma once

namespace mxl::proxy {
class ScopedRTScheduling {
  public:
    explicit ScopedRTScheduling(int prio);
    ~ScopedRTScheduling();

  private:
    int _policyBefore = -1;
    int _prioBefore = -1;
};
} // namespace mxl::proxy
