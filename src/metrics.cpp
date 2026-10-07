#include "metrics.hpp"
#include <array>
#include <cassert>
#include <cerrno>
#include <fcntl.h>
#include <filesystem>
#include <format>
#include <iomanip>
#include <mxl/time.h>
#include <spdlog/spdlog.h>
#include <sstream>
#include <sys/epoll.h>
#include <sys/eventfd.h>
#include <sys/socket.h>
#include <sys/un.h>
#include <system_error>
#include <unistd.h>

namespace mxl::proxy {

namespace {
// epoll_data tags. Sessions are numbered from 1, so neither collides.
constexpr std::uint64_t listenTag = 0;
constexpr std::uint64_t wakeTag = UINT64_MAX;
} // namespace

Metrics::Metrics(std::string const& socketPath, bool withNetworkLatency)
    : _socketPath(socketPath),
      _withNetworkLatency(withNetworkLatency) {
    _epollfd = ::epoll_create1(EPOLL_CLOEXEC);
    if (_epollfd < 0) {
        throw std::system_error{errno, std::generic_category(),
                                "epoll_create1"};
    }

    _listenFd = ::socket(AF_UNIX, SOCK_STREAM, 0);
    if (_listenFd < 0) {
        throw std::system_error{errno, std::generic_category(),
                                "socket(AF_UNIX,SOCK_STREAM,0)"};
    }

    ::sockaddr_un addr{};
    addr.sun_family = AF_UNIX;

    // sun_path is a fixed buffer and strncpy neither terminates it nor
    // complains: an over-long path silently binds a *truncated* one instead,
    // so the supervisor scrapes a socket nobody is listening on, and two
    // workers whose paths share a prefix collide on a path neither asked for.
    // That presents as EADDRINUSE from an unrelated worker, which is a long
    // way from the actual cause.
    if (socketPath.size() >= sizeof addr.sun_path) {
        throw std::system_error{
            ENAMETOOLONG, std::generic_category(),
            std::format("metrics socket path is {} bytes, the limit is {}",
                        socketPath.size(), sizeof addr.sun_path - 1)};
    }

    ::strncpy(addr.sun_path, socketPath.c_str(), sizeof addr.sun_path);

    // bind() fails with EADDRINUSE on a path that already exists, and a worker
    // killed with SIGKILL leaves its socket behind. A fresh work directory per
    // start is still the supervisor's job, but this removes the class of
    // failure rather than relying on it.
    if ((::unlink(socketPath.c_str()) < 0) && (errno != ENOENT)) {
        throw std::system_error{errno, std::generic_category(),
                                "unlink metrics socket"};
    }

    if (::bind(_listenFd, reinterpret_cast<::sockaddr*>(&addr), sizeof addr) <
        0) {
        throw std::system_error{errno, std::generic_category(), "bind"};
    }

    if (::listen(_listenFd, 16) < 0) {
        throw std::system_error{errno, std::generic_category(), "listen"};
    }

    if (::fcntl(_listenFd, F_SETFL, O_NONBLOCK) < 0) {
        throw std::system_error{errno, std::generic_category(),
                                "set O_NONBLOCK"};
    }

    auto ev = ::epoll_event{
        .events = EPOLLIN,
        .data = ::epoll_data{.u64 = listenTag},
    };
    if (::epoll_ctl(_epollfd, EPOLL_CTL_ADD, _listenFd, &ev) < 0) {
        throw std::system_error{errno, std::generic_category(),
                                "epoll_ctl (add)"};
    }

    _wakeFd = ::eventfd(0, EFD_CLOEXEC | EFD_NONBLOCK);
    if (_wakeFd < 0) {
        throw std::system_error{errno, std::generic_category(), "eventfd"};
    }

    auto wakeEv = ::epoll_event{
        .events = EPOLLIN,
        .data = ::epoll_data{.u64 = wakeTag},
    };
    if (::epoll_ctl(_epollfd, EPOLL_CTL_ADD, _wakeFd, &wakeEv) < 0) {
        throw std::system_error{errno, std::generic_category(),
                                "epoll_ctl (add wake fd)"};
    }

    _listenThread = std::thread([this]() { run(); });
}

Metrics::~Metrics() {
    // Stop the thread before releasing anything it uses. Closing _epollfd
    // first used to be the stop signal, but it does not wake a blocked
    // epoll_wait, so the thread could still accept a scrape during teardown.
    // accept() then returned the descriptor number just freed, epoll_ctl on
    // that number failed with EINVAL, and the throw from the thread aborted
    // the process before main could log why the worker was exiting.
    std::uint64_t one = 1;
    if (::write(_wakeFd, &one, sizeof one) < 0) {
        spdlog::error("failed to wake the metrics thread: {}",
                      ::strerror(errno));
    }
    _listenThread.join();

    for (auto& [_, session] : _sessions) {
        ::close(session.fd);
    }
    ::close(_listenFd);
    ::close(_wakeFd);
    ::close(_epollfd);
    std::error_code ec;
    std::filesystem::remove_all(_socketPath, ec);
}

void Metrics::observe(std::uint64_t bytes, std::uint64_t payloadBytes,
                      std::uint64_t grains, std::uint64_t skipped,
                      std::uint64_t sourceLatency,
                      std::uint64_t networkLatency) {
    std::lock_guard lock{_m};
    _totalBytes.add(bytes);
    _totalPayload.add(payloadBytes);
    _totalGrains.add(grains);
    _skipped.add(skipped);
    _sourceLatency.observe(sourceLatency);
    _networkLatency.observe(networkLatency);
}

void Metrics::run() {
    // Nothing may escape this thread: an uncaught exception here is
    // std::terminate for the whole worker, for the sake of a metrics scrape.
    try {
        serve();
    } catch (std::exception const& ex) {
        spdlog::error("metrics server stopped: {}", ex.what());
    }
}

void Metrics::serve() {
    std::array<::epoll_event, 16> events{};

    for (;;) {
        auto ret = ::epoll_wait(_epollfd, events.data(), events.size(), -1);
        if (ret < 0) {
            auto const error = errno;
            // SIGTERM may be delivered to this thread rather than the main one.
            if (error == EINTR) {
                continue;
            }
            spdlog::error("epoll err: {}", ::strerror(error));
            return;
        }

        for (auto i = 0; i < ret; ++i) {
            auto& ev = events[i];
            if (ev.data.u64 == wakeTag) {
                return;
            }
            if (ev.data.u64 == listenTag) {
                if (ev.events & EPOLLIN) {
                    accept();
                }
                continue;
            }

            if (ev.events & EPOLLERR || ev.events & EPOLLRDHUP) {
                removeSession(ev.data.u64);
            } else if (ev.events & EPOLLOUT) {
                writeable(ev.data.u64);
            }
        }
    }
}

void Metrics::accept() {
    for (;;) {
        auto sock = ::accept4(_listenFd, nullptr, nullptr,
                              SOCK_NONBLOCK | SOCK_CLOEXEC);
        if (sock < 0) {
            auto const error = errno;
            if (error == EINTR || error == ECONNABORTED) {
                continue;
            }
            if (error != EWOULDBLOCK && error != EAGAIN) {
                spdlog::error("metrics accept failed: {}", ::strerror(error));
            }
            return;
        }

        createSession(sock);
    }
}

void Metrics::createSession(int fd) {
    auto [it, created] =
        _sessions.emplace(++_sessionCounter, Session{fd, scrape(), 0});
    assert(created);

    ::epoll_event ev{.events = EPOLLOUT | EPOLLIN | EPOLLERR,
                     .data = ::epoll_data{.u64 = _sessionCounter}};
    if (::epoll_ctl(_epollfd, EPOLL_CTL_ADD, fd, &ev) < 0) {
        // One failed scrape is not a reason to take the worker down.
        spdlog::error("failed to add metrics client to epoll set: {}",
                      ::strerror(errno));
        _sessions.erase(it);
        ::close(fd);
    }
}

void Metrics::removeSession(std::uint64_t id) {
    // find, not operator[]: a missing id must not insert a default Session
    // and then close whatever descriptor its fd field holds.
    auto it = _sessions.find(id);
    if (it == _sessions.end()) {
        return;
    }
    ::epoll_ctl(_epollfd, EPOLL_CTL_DEL, it->second.fd, nullptr);
    ::close(it->second.fd);
    _sessions.erase(it);
}

void Metrics::writeable(std::uint64_t id) {
    auto it = _sessions.find(id);
    if (it == _sessions.end()) {
        return;
    }
    auto& session = it->second;
    for (;;) {
        if (session.written >= session.buf.size()) {
            removeSession(id);
            return;
        }
        auto out = ::write(session.fd, session.buf.data() + session.written,
                           session.buf.size() - session.written);
        if (out < 0) {
            auto const error = errno;
            if (error == EWOULDBLOCK || error == EAGAIN) {
                return;
            }

            spdlog::error("write error: {}", ::strerror(error));
            removeSession(id);
            return;
        }

        session.written += out;
    }
}

std::string Metrics::scrape() const noexcept {
    std::lock_guard lock{_m};
    try {
        std::stringstream ss{};
        ss << std::setprecision(std::numeric_limits<double>::digits10);
        ss << _totalBytes << _totalPayload << _totalGrains << _skipped
           << _sourceLatency;
        if (_withNetworkLatency) {
            ss << _networkLatency;
        }
        return ss.str();
    } catch (std::exception const& ex) {
        return std::format("scrape error: {}", ex.what());
    }
}

} // namespace mxl::proxy
