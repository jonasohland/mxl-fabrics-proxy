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
    Metrics(std::string const& socketPath, bool withNetworkLatency);
    ~Metrics();

    void observe(std::uint64_t bytes, std::uint64_t payloadBytes,
                 std::uint64_t grains, std::uint64_t skipped,
                 std::uint64_t sourceLatency, std::uint64_t networkLatency);

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
    Counter _totalPayload = makeCounter("mxl_payload_octets_total");
    Counter _totalBytes = makeCounter("mxl_octets_total");
    Counter _totalGrains = makeCounter("mxl_grains_total");
    Counter _skipped = makeCounter("mxl_grains_lost");
    Counter _lastGrainIndex = makeCounter("mxl_last_grain");
    Summary _sourceLatency =
        makeSummary("mxl_source_latency_ns", {0.01, 0.1, 0.5, 0.9, 0.99});
    Summary _networkLatency =
        makeSummary("mxl_network_latency_ns", {0.01, 0.1, 0.5, 0.9, 0.99});

    bool _withNetworkLatency;

    std::uint64_t _sessionCounter = 0;
    std::map<std::uint64_t, Session> _sessions = {};

    int _listenFd = 0;
    int _epollfd = 0;
};
} // namespace mxl::proxy
