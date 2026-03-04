#include "config.hpp"
#include "initiator.hpp"
#include "target.hpp"
#include "util.hpp"
#include "version.h"
#include <csignal>
#include <filesystem>
#include <format>
#include <fstream>
#include <iostream>
#include <mxl/fabrics.h>
#include <picojson.h>
#include <rdma/fabric.h>
#include <spdlog/sinks/stdout_color_sinks.h>
#include <spdlog/spdlog.h>
#include <sstream>
#include <system_error>

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
    std::cerr << "  -v, --version Print the version and exit\n";
    std::cerr << "  -h, --help    Print this help and exit\n";
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
        spdlog::error("fatal: {}", ex.what());
    } catch (std::exception const& ex) {
        spdlog::error("fatal: {}", ex.what());
        return 1;
    }

    return 0;
}
