/// mxl-mock-sink — a consumer for end-to-end tests.
///
/// Opens a flow, reads it, and checks the payload against the pattern
/// mxl-mock-src wrote. Prints a JSON summary and exits non-zero if what it read
/// is not what was sent.
///
/// The verification is the point. A replication path can move data and still be
/// wrong: the initiator computes scatter-gather offsets within the bounce
/// buffer ring from entrySize/entryCount, so a stale value writes into a
/// correctly registered region at the wrong offsets, the NIC reports nothing,
/// and the destination flow fills with garbage while every counter in the
/// system says healthy (§5.2). "The head index advanced" does not catch that.
/// Comparing bytes against a per-index pattern does.

#include "../mxl.hpp"
#include "../util.hpp"
#include "args.hpp"
#include "output.hpp"
#include "pattern.hpp"

#include <algorithm>
#include <csignal>
#include <format>
#include <iostream>
#include <limits>
#include <mxl/time.h>
#include <nlohmann/json.hpp>
#include <optional>
#include <string>
#include <variant>

namespace {

volatile std::sig_atomic_t exitSignalStatus = 0;

void signalHandler(int) {
    exitSignalStatus = 1;
}

void usage() {
    std::cerr
        << R"(mxl-mock-sink — read a flow and verify it, for end-to-end tests

Usage: mxl-mock-sink --domain PATH --flow-id UUID [options]

  --domain PATH       MXL domain directory.
  --flow-id UUID      The flow to read.

  --count N           Stop after N grains or sample batches. 0 (default) runs
                      until SIGTERM/SIGINT or the flow goes idle.
  --expect N          Fail unless at least N were read. Default: --count.
  --verify            Check the payload against --seed. Without it the tool
                      only reports that data arrived, not that it is correct.
  --seed N            Pattern seed, default 1. Must match mxl-mock-src.
  --timeout MS        Per-read timeout, default 1000.
  --idle-timeout MS   Give up after this long with nothing to read. Default
                      10000; 0 waits indefinitely.
  --wait-for-flow MS  Wait this long for the flow to appear before starting.
                      Default 10000. A destination flow is created by the
                      receiving worker, so it does not exist until the session
                      is up.
  --json              Print a JSON summary on stdout at exit.
  --verbose           Log every grain.
  --help

Exit: 0 if everything expected arrived and verified, 1 otherwise, 2 on a usage
error.
)";
}

struct Options {
    std::string domain;
    std::string flowId;
    std::uint64_t count = 0;
    std::uint64_t expect = 0;
    bool verify = false;
    std::uint64_t seed = 1;
    std::uint64_t timeout = 1000;
    std::uint64_t idleTimeout = 10000;
    std::uint64_t waitForFlow = 10000;
    bool json = false;
    bool verbose = false;
};

struct Summary {
    std::string format;
    std::uint64_t read = 0;
    std::uint64_t gaps = 0;
    std::uint64_t mismatched = 0;
    std::optional<std::uint64_t> firstIndex;
    std::uint64_t lastIndex = 0;
    std::uint64_t bytes = 0;
    std::uint64_t latencyMin = std::numeric_limits<std::uint64_t>::max();
    std::uint64_t latencyMax = 0;
    std::uint64_t latencyTotal = 0;
    bool signalled = false;
    std::string failure;

    void observeLatency(std::uint64_t ns) {
        latencyMin = std::min(latencyMin, ns);
        latencyMax = std::max(latencyMax, ns);
        latencyTotal += ns;
    }

    [[nodiscard]] std::uint64_t latencyMean() const {
        return read == 0 ? 0 : latencyTotal / read;
    }
};

/// The destination flow of a replication session is created by the
/// receiving worker, so a sink started alongside the session finds nothing
/// for a moment. Retrying here rather than making the caller sleep keeps
/// that timing out of every test script.
std::variant<::mxl::DiscreteFlowReader, ::mxl::ContinuousFlowReader>
openFlow(::mxl::Instance const& instance, Options const& options,
         utils::ExitSignal sig) {
    auto const deadline = ::mxlGetTime() + (options.waitForFlow * 1'000'000);
    for (;;) {
        try {
            return instance.openFlow(options.flowId);
        } catch (::mxl::Exception const& ex) {
            if (sig.shouldExit() || ::mxlGetTime() >= deadline) {
                throw;
            }
            ::mxlSleepUntil(::mxlGetTime() + 50'000'000);
        }
    }
}

std::uint64_t sourceLatency(::mxlRational const& rate, std::uint64_t index) {
    auto const now = ::mxlGetTime();
    auto const grainTime = ::mxlIndexToTimestamp(&rate, index);
    return now > grainTime ? now - grainTime : 0;
}

void readGrains(::mxl::DiscreteFlowReader reader, Options const& options,
                utils::ExitSignal sig, Summary& summary) {
    auto const rate = reader.getRate();
    auto index = reader.getHeadIndex();
    auto lastProgress = ::mxlGetTime();

    while (!sig.shouldExit()) {
        if (options.count != 0 && summary.read >= options.count) {
            break;
        }

        try {
            auto const grain = reader.getGrain(
                index, std::chrono::milliseconds(options.timeout));

            if (options.verify) {
                auto const at = ::mxl::mock::verifyGrain(
                    grain.data(), grain.size(), options.seed, index);
                if (at != grain.size()) {
                    ++summary.mismatched;
                    std::cerr << std::format(
                        "payload mismatch at index={} offset={}\n", index, at);
                }
            }

            if (!summary.firstIndex) {
                summary.firstIndex = index;
            }
            summary.lastIndex = index;
            summary.bytes += grain.size();
            summary.observeLatency(sourceLatency(rate, index));
            ++summary.read;
            if (options.verbose) {
                std::cerr << std::format("grain index={} size={}\n", index,
                                         grain.size());
            }

            lastProgress = ::mxlGetTime();
            ++index;
        } catch (::mxl::Exception const& ex) {
            if (ex.isTooLate()) {
                // Fell behind the producer. Resync to the head and record
                // what was missed rather than pretending it arrived.
                auto const head = reader.getHeadIndex();
                if (head > index) {
                    summary.gaps += head - index;
                }
                index = head;
                lastProgress = ::mxlGetTime();
                continue;
            }
            if (ex.isTooEarly() || ex.isTimeout()) {
                if (options.idleTimeout != 0 &&
                    ::mxlGetTime() - lastProgress >
                        options.idleTimeout * 1'000'000) {
                    summary.failure =
                        std::format("no grain for {} ms", options.idleTimeout);
                    return;
                }
                continue;
            }
            throw;
        }
    }
}

void readSamples(::mxl::ContinuousFlowReader reader, Options const& options,
                 utils::ExitSignal sig, Summary& summary) {
    auto const rate = reader.getRate();
    auto const batch = reader.getBatchSize();
    auto index = reader.getHeadIndex();
    auto lastProgress = ::mxlGetTime();

    while (!sig.shouldExit()) {
        if (options.count != 0 && summary.read >= options.count) {
            break;
        }

        try {
            auto const slice = reader.getSamples(
                index, batch, std::chrono::milliseconds(options.timeout));
            auto const total =
                slice.base.fragments[0].size + slice.base.fragments[1].size;
            auto const sampleSize = batch == 0 ? 0 : total / batch;

            if (options.verify && sampleSize != 0) {
                std::size_t offset = 0;
                for (auto const& fragment : slice.base.fragments) {
                    if (fragment.size == 0) {
                        continue;
                    }
                    auto const samples = fragment.size / sampleSize;
                    for (std::size_t channel = 0; channel < slice.count;
                         ++channel) {
                        auto const* base =
                            static_cast<std::uint8_t const*>(fragment.pointer) +
                            (channel * slice.stride);
                        for (std::size_t sample = 0; sample < samples;
                             ++sample) {
                            auto const expected = ::mxl::mock::sampleValue(
                                options.seed, channel, index + offset + sample);
                            if (!::mxl::mock::verifySample(
                                    base + (sample * sampleSize), sampleSize,
                                    expected)) {
                                ++summary.mismatched;
                                std::cerr << std::format(
                                    "sample mismatch index={} channel={} "
                                    "sample={}\n",
                                    index, channel, offset + sample);
                                break;
                            }
                        }
                    }
                    offset += samples;
                }
            }

            if (!summary.firstIndex) {
                summary.firstIndex = index;
            }
            summary.lastIndex = index;
            summary.bytes += total * slice.count;
            summary.observeLatency(sourceLatency(rate, index));
            ++summary.read;
            if (options.verbose) {
                std::cerr << std::format(
                    "samples index={} count={} channels={}\n", index, batch,
                    slice.count);
            }

            lastProgress = ::mxlGetTime();
            index += batch;
        } catch (::mxl::Exception const& ex) {
            if (ex.isTooLate()) {
                auto const head = reader.getHeadIndex();
                if (head > index) {
                    summary.gaps += head - index;
                }
                index = head;
                lastProgress = ::mxlGetTime();
                continue;
            }
            if (ex.isTooEarly() || ex.isTimeout()) {
                if (options.idleTimeout != 0 &&
                    ::mxlGetTime() - lastProgress >
                        options.idleTimeout * 1'000'000) {
                    summary.failure = std::format("no samples for {} ms",
                                                  options.idleTimeout);
                    return;
                }
                continue;
            }
            throw;
        }
    }
}

} // namespace

int main(int argc, char* argv[]) {
    using namespace mxl::mock;

    Options options;
    try {
        auto const args =
            Args::parse(argc, argv,
                        {"domain", "flow-id", "count", "expect", "verify",
                         "seed", "timeout", "idle-timeout", "wait-for-flow",
                         "json", "verbose", "help"});
        if (args.flag("help")) {
            usage();
            return 0;
        }
        options.domain = args.required("domain");
        options.flowId = args.required("flow-id");
        options.count = args.num("count", 0);
        options.expect = args.num("expect", options.count);
        options.verify = args.flag("verify");
        options.seed = args.num("seed", 1);
        options.timeout = args.num("timeout", 1000);
        options.idleTimeout = args.num("idle-timeout", 10000);
        options.waitForFlow = args.num("wait-for-flow", 10000);
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
        auto const instance = ::mxl::Instance{options.domain};
        auto reader = openFlow(instance, options, sig);

        if (std::holds_alternative<::mxl::DiscreteFlowReader>(reader)) {
            summary.format = "discrete";
            std::cerr << std::format("consuming discrete flow {}\n",
                                     options.flowId);
            readGrains(std::move(std::get<::mxl::DiscreteFlowReader>(reader)),
                       options, sig, summary);
        } else {
            summary.format = "continuous";
            std::cerr << std::format("consuming continuous flow {}\n",
                                     options.flowId);
            readSamples(
                std::move(std::get<::mxl::ContinuousFlowReader>(reader)),
                options, sig, summary);
        }
    } catch (std::exception const& ex) {
        summary.failure = ex.what();
        std::cerr << std::format("fatal: {}\n", ex.what());
    }

    summary.signalled = exitSignalStatus != 0;

    auto ok = summary.failure.empty() && summary.mismatched == 0;
    if (options.expect != 0 && summary.read < options.expect) {
        ok = false;
        if (summary.failure.empty()) {
            summary.failure = std::format("read {} of {} expected",
                                          summary.read, options.expect);
        }
    }

    std::cerr << std::format(
        "read {} gaps={} mismatched={}{}\n", summary.read, summary.gaps,
        summary.mismatched,
        summary.failure.empty() ? "" : (" failure=" + summary.failure));

    if (options.json) {
        guard.restore();
        auto const report = nlohmann::json{
            {"flow_id", options.flowId},
            {"format", summary.format},
            {"read", summary.read},
            {"gaps", summary.gaps},
            {"verified",
             options.verify ? summary.read - summary.mismatched : 0},
            {"mismatched", summary.mismatched},
            {"first_index", summary.firstIndex.value_or(0)},
            {"last_index", summary.lastIndex},
            {"bytes", summary.bytes},
            {"latency_ns",
             {{"min", summary.read == 0 ? 0 : summary.latencyMin},
              {"max", summary.latencyMax},
              {"mean", summary.latencyMean()}}},
            {"signalled", summary.signalled},
            {"failure", summary.failure},
            {"ok", ok},
        };
        std::cout << report.dump() << "\n";
    }

    return ok ? 0 : 1;
}
