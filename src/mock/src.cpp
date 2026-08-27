/// mxl-mock-src — a producer for end-to-end tests.
///
/// Creates a flow from an NMOS flow definition and writes to it at the flow's
/// own edit rate, with a payload that mxl-mock-sink can verify byte for byte.
/// It is what a media function would be in a real deployment, minus the media.
///
/// mxl-utils' testutil builds synthetic flows on disk that pkg/mxl can open,
/// which is the right tool for unit-testing the agent's inventory logic. It
/// cannot exercise the SDK path — flow creation, the ring buffer, grain commit
/// semantics, the timing model — and that path is what a replication end-to-end
/// test is actually testing. Hence a producer that links the real library.
///
/// Everything except the summary goes to stderr; see output.hpp for why that
/// needs enforcing.

#include "../mxl.hpp"
#include "../util.hpp"
#include "args.hpp"
#include "output.hpp"
#include "pattern.hpp"

#include <chrono>
#include <csignal>
#include <cstring>
#include <format>
#include <fstream>
#include <iostream>
#include <mxl/time.h>
#include <nlohmann/json.hpp>
#include <sstream>
#include <string>
#include <variant>

namespace {

volatile std::sig_atomic_t exitSignalStatus = 0;

void signalHandler(int) {
    exitSignalStatus = 1;
}

void usage() {
    std::cerr
        << R"(mxl-mock-src — write a flow at its own rate, for end-to-end tests

Usage: mxl-mock-src --domain PATH --flow-def FILE [options]

  --domain PATH       MXL domain directory. Must already exist.
  --flow-def FILE     NMOS flow definition JSON. The flow is created from it.

  --count N           Stop after N grains or sample batches. 0 (default) runs
                      until SIGTERM/SIGINT.
  --seed N            Payload pattern seed, default 1. mxl-mock-sink must be
                      given the same one to verify.
  --zero              Write zeroes instead of the pattern. Faster, and verifies
                      nothing about where the bytes landed.
  --pause-after N     Stop writing after N grains, then resume. Drives the
                      PAUSED -> ACTIVE transition a replication path reports.
  --pause-for MS      How long to pause, default 2000.
  --start-index N     First index to write. Default is the current index for
                      the flow's rate, which is what a live producer does.
  --json              Print a JSON summary on stdout at exit.
  --verbose           Log every grain.
  --help

Exit: 0 on a clean finish or signal, 1 on failure, 2 on a usage error.
)";
}

std::string readFile(std::string const& path) {
    std::ifstream stream{path, std::ios::binary};
    if (!stream) {
        throw std::runtime_error{std::format("cannot read {}", path)};
    }
    std::ostringstream buffer;
    buffer << stream.rdbuf();
    return buffer.str();
}

struct Options {
    std::string domain;
    std::string flowDef;
    std::uint64_t count = 0;
    std::uint64_t seed = 1;
    bool zero = false;
    std::uint64_t pauseAfter = 0;
    std::uint64_t pauseFor = 2000;
    bool haveStartIndex = false;
    std::uint64_t startIndex = 0;
    bool json = false;
    bool verbose = false;
};

struct Summary {
    std::string flowId;
    std::string format;
    std::uint64_t written = 0;
    std::uint64_t firstIndex = 0;
    std::uint64_t lastIndex = 0;
    std::uint64_t bytes = 0;
    bool signalled = false;
};

/// Sleeps until the timestamp of `index`, in slices, so that a signal is
/// noticed promptly even at a slow edit rate.
void sleepUntilIndex(::mxlRational const& rate, std::uint64_t index,
                     utils::ExitSignal sig) {
    auto const target = ::mxlIndexToTimestamp(&rate, index);
    for (;;) {
        if (sig.shouldExit()) {
            return;
        }
        auto const now = ::mxlGetTime();
        if (now >= target) {
            return;
        }
        auto const remaining = target - now;
        constexpr std::uint64_t slice = 50'000'000; // 50 ms
        ::mxlSleepUntil(now + (remaining < slice ? remaining : slice));
    }
}

void pause(Options const& options, ::mxlRational const& rate,
           std::uint64_t& index, utils::ExitSignal sig) {
    std::cerr << std::format("pausing for {} ms\n", options.pauseFor);
    auto const until = ::mxlGetTime() + (options.pauseFor * 1'000'000);
    while (!sig.shouldExit() && ::mxlGetTime() < until) {
        ::mxlSleepUntil(::mxlGetTime() + 20'000'000);
    }
    // Resume at the *current* index rather than the stale one. Catching up
    // by bursting out every grain the pause skipped is not what a producer
    // that stopped and restarted does, and it would hide exactly the
    // head-index gap a paused source is supposed to show.
    index = ::mxlGetCurrentIndex(&rate);
    std::cerr << std::format("resuming at index {}\n", index);
}

void writeGrains(::mxl::DiscreteFlowWriter writer, Options const& options,
                 utils::ExitSignal sig, Summary& summary) {
    auto const rate = writer.getRate();
    auto index = options.haveStartIndex ? options.startIndex
                                        : ::mxlGetCurrentIndex(&rate);
    summary.firstIndex = index;

    while (!sig.shouldExit()) {
        if (options.count != 0 && summary.written >= options.count) {
            break;
        }
        if (options.pauseAfter != 0 && summary.written == options.pauseAfter) {
            pause(options, rate, index, sig);
            if (sig.shouldExit()) {
                break;
            }
        }

        {
            auto access = writer.openGrain(index);
            if (options.zero) {
                std::memset(access.data(), 0, access.size());
            } else {
                ::mxl::mock::fillGrain(access.data(), access.size(),
                                       options.seed, index);
            }
            // A reader blocks until every slice is valid, so a grain
            // committed without this is a grain nothing will ever read.
            access.validSlices(access.totalSlices());
            summary.bytes += access.size();
            if (options.verbose) {
                std::cerr << std::format("grain index={} size={}\n", index,
                                         access.size());
            }
            // Committed when access is destroyed.
        }

        summary.lastIndex = index;
        ++summary.written;
        ++index;
        sleepUntilIndex(rate, index, sig);
    }
}

void writeSamples(::mxl::ContinuousFlowWriter writer, Options const& options,
                  utils::ExitSignal sig, Summary& summary) {
    auto const rate = writer.getRate();
    auto const batch = writer.getBatchSize();
    auto index = options.haveStartIndex ? options.startIndex
                                        : ::mxlGetCurrentIndex(&rate);
    summary.firstIndex = index;

    while (!sig.shouldExit()) {
        if (options.count != 0 && summary.written >= options.count) {
            break;
        }
        if (options.pauseAfter != 0 && summary.written == options.pauseAfter) {
            pause(options, rate, index, sig);
            if (sig.shouldExit()) {
                break;
            }
        }

        {
            auto access = writer.openSamples(index, batch);
            auto const& buffers = access.buffers();
            auto const total =
                buffers.base.fragments[0].size + buffers.base.fragments[1].size;
            auto const sampleSize = batch == 0 ? 0 : total / batch;

            if (sampleSize != 0) {
                std::size_t offset = 0;
                for (auto const& fragment : buffers.base.fragments) {
                    if (fragment.size == 0) {
                        continue;
                    }
                    auto const samples = fragment.size / sampleSize;
                    for (std::size_t channel = 0; channel < buffers.count;
                         ++channel) {
                        auto* base =
                            static_cast<std::uint8_t*>(fragment.pointer) +
                            (channel * buffers.stride);
                        for (std::size_t sample = 0; sample < samples;
                             ++sample) {
                            auto const value =
                                options.zero ? 0
                                             : ::mxl::mock::sampleValue(
                                                   options.seed, channel,
                                                   index + offset + sample);
                            ::mxl::mock::fillSample(base +
                                                        (sample * sampleSize),
                                                    sampleSize, value);
                        }
                    }
                    offset += samples;
                }
            }
            summary.bytes += total * buffers.count;
            if (options.verbose) {
                std::cerr << std::format(
                    "samples index={} count={} channels={}\n", index, batch,
                    buffers.count);
            }
            // Committed when access is destroyed.
        }

        summary.lastIndex = index;
        ++summary.written;
        index += batch;
        sleepUntilIndex(rate, index, sig);
    }
}

} // namespace

int main(int argc, char* argv[]) {
    using namespace mxl::mock;

    Options options;
    try {
        auto const args = Args::parse(
            argc, argv,
            {"domain", "flow-def", "count", "seed", "zero", "pause-after",
             "pause-for", "start-index", "json", "verbose", "help"});
        if (args.flag("help")) {
            usage();
            return 0;
        }
        options.domain = args.required("domain");
        options.flowDef = args.required("flow-def");
        options.count = args.num("count", 0);
        options.seed = args.num("seed", 1);
        options.zero = args.flag("zero");
        options.pauseAfter = args.num("pause-after", 0);
        options.pauseFor = args.num("pause-for", 2000);
        options.haveStartIndex = args.has("start-index");
        options.startIndex = args.num("start-index", 0);
        options.json = args.flag("json");
        options.verbose = args.flag("verbose");
    } catch (UsageError const& ex) {
        std::cerr << std::format("usage error: {}\n\n", ex.what());
        usage();
        return 2;
    }

    std::signal(SIGTERM, signalHandler);
    std::signal(SIGINT, signalHandler);
    auto const sig = utils::ExitSignal{&exitSignalStatus};

    Summary summary;
    StdoutGuard guard;

    try {
        auto const definition = readFile(options.flowDef);
        summary.flowId = nlohmann::json::parse(definition).value("id", "");

        auto const instance = ::mxl::Instance{options.domain};
        auto flow = instance.createFlow(definition);

        if (std::holds_alternative<::mxl::DiscreteFlowWriter>(flow)) {
            summary.format = "discrete";
            std::cerr << std::format("producing discrete flow {}\n",
                                     summary.flowId);
            writeGrains(std::move(std::get<::mxl::DiscreteFlowWriter>(flow)),
                        options, sig, summary);
        } else {
            summary.format = "continuous";
            std::cerr << std::format("producing continuous flow {}\n",
                                     summary.flowId);
            writeSamples(std::move(std::get<::mxl::ContinuousFlowWriter>(flow)),
                         options, sig, summary);
        }
    } catch (std::exception const& ex) {
        std::cerr << std::format("fatal: {}\n", ex.what());
        return 1;
    }

    summary.signalled = exitSignalStatus != 0;
    std::cerr << std::format("wrote {} of format {}\n", summary.written,
                             summary.format);

    if (options.json) {
        guard.restore();
        auto const report = nlohmann::json{
            {"flow_id", summary.flowId},
            {"format", summary.format},
            {"written", summary.written},
            {"first_index", summary.firstIndex},
            {"last_index", summary.lastIndex},
            {"bytes", summary.bytes},
            {"seed", options.seed},
            {"signalled", summary.signalled},
        };
        std::cout << report.dump() << "\n";
    }
    return 0;
}
