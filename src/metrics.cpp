#include "metrics.hpp"
#include <array>
#include <cassert>
#include <cerrno>
#include <fcntl.h>
#include <filesystem>
#include <iomanip>
#include <mxl/time.h>
#include <spdlog/spdlog.h>
#include <sstream>
#include <sys/epoll.h>
#include <sys/socket.h>
#include <sys/un.h>
#include <system_error>

namespace mxl::proxy {

Metrics::Metrics(std::string const& socketPath)
    : _socketPath(socketPath) {
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
    ::strncpy(addr.sun_path, socketPath.c_str(), socketPath.length());

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
        .data = ::epoll_data{.u64 = 0},
    };
    if (::epoll_ctl(_epollfd, EPOLL_CTL_ADD, _listenFd, &ev) < 0) {
        throw std::system_error{errno, std::generic_category(),
                                "epoll_ctl (add)"};
    }

    _listenThread = std::thread([this]() { run(); });
}

Metrics::~Metrics() {
    for (auto& [_, session] : _sessions) {
        ::close(session.fd);
    }

    ::close(_epollfd);
    _listenThread.join();
    std::filesystem::remove_all(_socketPath);
}

void Metrics::observe(std::uint64_t bytes, std::uint64_t payloadBytes,
                      std::uint64_t grains, std::uint64_t skipped,
                      std::uint64_t latencyIn) {
    std::lock_guard lock{_m};
    _totalBytes.add(bytes);
    _totalPayload.add(payloadBytes);
    _totalGrains.add(grains);
    _skipped.add(skipped);
    _latency.add(latencyIn);
    _latencySummary.observe(latencyIn);
}

void Metrics::run() {
    std::array<::epoll_event, 16> events{};

    for (;;) {
        auto ret = ::epoll_wait(_epollfd, events.data(), events.size(), 1000);
        if (ret < 0) {
            spdlog::info("epoll err: {}", ::strerror(errno));
            return;
        }

        for (auto i = 0; i < ret; ++i) {
            auto& ev = events[i];
            if (ev.data.u64 == 0) {
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
    ::sockaddr_un addr;
    ::socklen_t len = sizeof addr;
    for (;;) {
        auto sock =
            ::accept(_listenFd, reinterpret_cast<::sockaddr*>(&addr), &len);
        if (sock < 0) {
            auto const error = errno;
            if (error == EWOULDBLOCK || error == EAGAIN) {
                return;
            }

            throw std::system_error{error, std::generic_category(),
                                    "accept failed"};
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
    if (::fcntl(it->second.fd, F_SETFL, O_NONBLOCK) < 0) {
        _sessions.erase(_sessionCounter);
        ::close(fd);
        throw std::system_error{errno, std::generic_category(),
                                "set O_NONBLOCK on client socket"};
    }
    if (::epoll_ctl(_epollfd, EPOLL_CTL_ADD, it->second.fd, &ev) < 0) {
        _sessions.erase(_sessionCounter);
        ::close(fd);
        throw std::system_error{errno, std::generic_category(),
                                "add client socket to epoll set"};
    }
}

void Metrics::removeSession(std::uint64_t id) {
    auto& session = _sessions[id];
    ::epoll_ctl(_epollfd, EPOLL_CTL_DEL, session.fd, {});
    ::close(session.fd);
    _sessions.erase(id);
}

void Metrics::writeable(std::uint64_t id) {
    auto& session = _sessions[id];
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
        ss << _totalBytes << _totalPayload << _totalGrains << _latency
           << _latencySummary;
        return ss.str();
    } catch (std::exception const& ex) {
        return std::format("scrape error: {}", ex.what());
    }
}

} // namespace mxl::proxy
