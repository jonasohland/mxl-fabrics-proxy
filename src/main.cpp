#include "config.hpp"
#include "deferred.hpp"
#include "fabrics.hpp"
#include "initiator.hpp"
#include "mxl.hpp"
#include "target.hpp"
#include "util.hpp"
#include "version.h"
#include <csignal>
#include <cstdlib>
#include <filesystem>
#include <format>
#include <fstream>
#include <iostream>
#include <mxl/fabrics.h>
#include <nlohmann/json.hpp>
#include <picojson.h>
#include <rdma/fabric.h>
#include <spdlog/sinks/stdout_color_sinks.h>
#include <spdlog/spdlog.h>
#include <sstream>
#include <system_error>
#include <unistd.h>

namespace {
volatile std::sig_atomic_t exitSignalStatus = 0;
void signalHandler(int signal) {
    exitSignalStatus = signal;
}
} // namespace

std::string readFile(std::filesystem::path filename) {
    std::ifstream ifs{filename};
    std::stringstream ss{};
    if (!ifs.is_open()) {
        throw std::system_error{errno, std::generic_category(),
                                std::format("open {}", filename.string())};
    }
    ss << ifs.rdbuf();
    if (ifs.bad()) {
        throw std::system_error{
            errno, std::generic_category(),
            std::format("read file at {}", filename.string())};
    }
    return ss.str();
}

void printHelp(char const* programName) {
    std::cerr << "Usage: " << programName << " [OPTIONS] <CONFIG-FILE>\n";
    std::cerr << "Options:\n";
    std::cerr << "  -v, --version    Print the version and exit\n";
    std::cerr << "      --interfaces Print the available fabric interfaces as "
                 "JSON on stdout and exit\n";
    std::cerr << "  -h, --help       Print this help and exit\n";
}

/// Enumerate the fabric interfaces libfabric offers on this host and print them
/// as a JSON array on stdout.
///
/// This exists because a supervisor cannot enumerate them itself without
/// linking libfabric, and guessing from device nodes and interface names is a
/// heuristic whose failure mode is a confusing restart loop. Asking the same
/// binary that will run the transfer is ground truth.
int printInterfaces() {
    // mxlFabricsGetInterfaces() needs a fabrics instance, which needs an mxl
    // instance, which needs a domain directory that already exists. The probe
    // never touches the domain, so use a throwaway one rather than requiring
    // the caller to name a real domain just to ask what the hardware can do.
    auto const domainPath =
        std::filesystem::temp_directory_path() /
        std::format("mxl-fabrics-proxy-worker.interfaces.{}", ::getpid());

    auto ec = std::error_code{};
    std::filesystem::create_directories(domainPath, ec);
    if (ec) {
        std::cerr << std::format(
            "fatal: failed to create temporary domain {}: {}\n",
            domainPath.string(), ec.message());
        return 1;
    }
    auto const cleanup = mxl::defer([&domainPath]() {
        auto ec = std::error_code{};
        std::filesystem::remove_all(domainPath, ec);
    });

    // The worker logs to stdout, and libfabric's own diagnostics are routed
    // there too, so a warning would land in the middle of the JSON. Point
    // stdout at stderr for the duration of the probe and restore it before
    // printing: diagnostics stay visible for whoever is debugging, and the
    // data stream stays parseable without depending on how noisy this host's
    // libfabric happens to be.
    auto interfaces = std::vector<mxl::fabrics::InterfaceInfo>{};
    try {
        ::fflush(stdout);
        auto const savedStdout = ::dup(STDOUT_FILENO);
        if (savedStdout < 0) {
            throw std::system_error{errno, std::generic_category(), "dup"};
        }
        if (::dup2(STDERR_FILENO, STDOUT_FILENO) < 0) {
            ::close(savedStdout);
            throw std::system_error{errno, std::generic_category(), "dup2"};
        }
        auto const restore = mxl::defer([savedStdout]() {
            ::fflush(stdout);
            ::dup2(savedStdout, STDOUT_FILENO);
            ::close(savedStdout);
        });

        auto const instance =
            mxl::fabrics::Instance{mxl::Instance{domainPath.string()}};
        interfaces = instance.getInterfaces();
    } catch (std::exception const& ex) {
        std::cerr << std::format("fatal: {}\n", ex.what());
        return 1;
    }

    try {
        auto out = nlohmann::json::array();
        for (auto const& iface : interfaces) {
            auto entry = nlohmann::json{
                {"provider",
                 std::string{mxl::fabrics::providerName(iface.provider)}},
                {"node", iface.node},
                {"service", iface.service},
                {"caps",
                 {
                     {"flags", mxl::fabrics::capsFlagNames(iface.capsFlags)},
                     {"max_message_size", iface.maxMessageSize},
                 }},
            };

            // Passed through verbatim. The library documents these attributes
            // as best effort and their contents vary by platform and hardware,
            // so the consumer decides what it can use. `device_name` is the
            // only one that names a physical interface, and it is not always
            // present — notably, there is no dedicated interface-name field in
            // the API at all.
            if (!iface.attr.empty()) {
                auto attr = nlohmann::json::parse(iface.attr, nullptr, false);
                if (!attr.is_discarded()) {
                    entry["attr"] = std::move(attr);
                }
            }

            out.push_back(std::move(entry));
        }

        std::cout << out.dump() << "\n";
    } catch (std::exception const& ex) {
        std::cerr << std::format("fatal: {}\n", ex.what());
        return 1;
    }

    return 0;
}

void printVersion() {
    ::mxlVersionType mxlVersion;
    ::mxlGetVersion(&mxlVersion);
    auto fiVersion = ::fi_version();
    std::cerr << "proxy     " << mxl::proxy::version << "\n";
    std::cerr << "mxl       " << mxlVersion.full << "\n";
    std::cerr << "libfabric " << FI_MAJOR(fiVersion) << "."
              << FI_MINOR(fiVersion) << "\n";
}

int main(int argc, char* argv[]) {
    std::string configFilePath{};

    // At least 1 arg (config file) is required.
    if (argc < 2) {
        printHelp(argv[0]);
        return 1;
    }

    // Worst argument parser of the decade.
    for (int i = 1; i < argc; ++i) {
        auto arg = std::string{argv[i]};
        if (arg.starts_with("-")) {
            if (arg == "-v" || arg == "--version") {
                printVersion();
                return 0;
            }
            if (arg == "-h" || arg == "--help") {
                printHelp(argv[0]);
                return 0;
            }
            if (arg == "--interfaces") {
                return printInterfaces();
            }
        } else {
            if (!configFilePath.empty()) {
                printHelp(argv[0]);
                return 1;
            }
            configFilePath = arg;
        }
    }
    if (configFilePath.empty()) {
        printHelp(argv[0]);
        return 1;
    }

    std::signal(SIGTERM, signalHandler);
    std::signal(SIGINT, signalHandler);
    auto signal = utils::ExitSignal{&exitSignalStatus};

    try {
        auto config = mxl::proxy::Config::parse(readFile(configFilePath));
        if (config.isInitiator()) {
            mxl::proxy::Initiator::run(std::move(config), signal);
        } else {
            mxl::proxy::Target::run(std::move(config), signal);
        }
    } catch (::mxl::Exception const& ex) {
        if (ex.isInterrupted()) {
            spdlog::info("interrupted, exiting");
            return 0;
        }
        // Every non-interrupt exception is a failure, so say so in the exit
        // status. Nothing classifies failures from the exit code — a
        // supervisor cannot, since config errors and transient timeouts both
        // arrive as mxl::Exception — but reporting success after printing
        // `fatal:` misleads systemd, CI and anyone running this by hand.
        spdlog::error("fatal: {}", ex.what());
        return 1;
    } catch (std::exception const& ex) {
        spdlog::error("fatal: {}", ex.what());
        return 1;
    }

    return 0;
}
