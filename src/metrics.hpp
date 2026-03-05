#pragma once

#include "summary.hpp"
#include <cstdint>
#include <map>
#include <mutex>
#include <string>
#include <thread>

namespace mxl::proxy {

class Metrics {
  public:
    Metrics(std::string const& socketPath);
    ~Metrics();

    void observe(std::uint64_t bytes, std::uint64_t payloadBytes,
                 std::uint64_t grains, std::uint64_t latencyIn);

  private:
    struct Session {
        int fd;
        std::string buf;
        std::size_t written;
    };

  private:
    [[nodiscard]] std::string scrape() const noexcept;
    void run();
    void accept();
    void createSession(int);
    void removeSession(std::uint64_t);
    void writeable(std::uint64_t);

    std::string _socketPath;
    std::thread _listenThread;

    mutable std::mutex _m{};
    Counter _totalPayload = makeCounter("totalPayload");
    Counter _totalBytes = makeCounter("totalBytes");
    Counter _totalGrains = makeCounter("totalGrains");
    Counter _latency = makeCounter("lantecy");
    Summary _latencySummary =
        makeSummary("latency", {0.01, 0.1, 0.5, 0.9, 0.99});

    std::uint64_t _sessionCounter = 0;
    std::map<std::uint64_t, Session> _sessions = {};

    int _listenFd = 0;
    int _epollfd = 0;
};
} // namespace mxl::proxy
