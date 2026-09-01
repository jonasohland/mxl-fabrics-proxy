#include "initiator.hpp"
#include "fabrics.hpp"
#include "rt.hpp"
#include "util.hpp"
#include <chrono>
#include <mxl/time.h>
#include <optional>
#include <spdlog/spdlog.h>

namespace mxl::proxy {
namespace {

/// The connect loop, shared by both flow kinds: they differ only in the
/// initiator type. Returns once connected or once signalled; throws when the
/// configured connect timeout expires.
template <typename Initiator>
void connectLoop(Initiator &initiator, utils::ExitSignal sig,
                 std::optional<std::chrono::milliseconds> timeout) {
  spdlog::info("waiting to connect...");
  auto const start = std::chrono::steady_clock::now();
  for (;;) {
    if (sig.shouldExit()) {
      return;
    }

    if (initiator.makeProgress(std::chrono::milliseconds(500))) {
      if (timeout && ((std::chrono::steady_clock::now() - start) > *timeout)) {
        throw Exception{MXL_ERR_TIMEOUT,
                        "timed out waiting to connect to the target"};
      }
      continue;
    }

    spdlog::info("connected");
    return;
  }
}

/// How long the source flow may go without a grain before the initiator gives
/// up the latency-measurement writer it co-holds on that flow.
///
/// The writer exists only to stamp a tx timestamp into each grain header
/// (§12), and it takes a *shared* flock on the flow's data and grain files for
/// as long as it is open. That lock is what stops the flow's real owner from
/// cleaning up: `mxl::lib::Instance::releaseWriter` deletes the flow directory
/// only if it can upgrade to an exclusive lock, so a target worker shutting
/// down while some initiator still reads its flow leaves the directory behind.
/// The orphan is still discovered by the destination node's inventory, so a
/// request selecting that domain keeps matching it and its path sits in PAUSED
/// forever — and the worker holding the lock is the very one that path is
/// running, which is what makes it self-sustaining rather than transient.
///
/// Dropping the writer while nothing is flowing breaks that. Release is
/// deliberately routed through the ordinary MXL path rather than any cleanup
/// of our own: the release either finds another writer holding the flow — an
/// ordinary pause, the owner is alive, nothing is deleted — or finds itself
/// alone, which is MXL's own definition of an orphan and exactly the criterion
/// `mxlGarbageCollectFlows` applies. This is not new authority in kind. The
/// same destructor already deletes the orphan when the session is torn down
/// for a long-idle source; this only stops that from being the *first*
/// opportunity, which the server takes minutes to reach (§11.1).
///
/// It does widen the window, and the consequence is worth naming: a source
/// flow whose producer exits while an initiator is attached is now removed a
/// second later rather than at worker exit — including in an area granted
/// `read` only (§10.6, §13). The grant is not the thing that permits it; the
/// writer this initiator has held all along is, and that writer is the actual
/// defect. Closing it properly means the initiator never co-writing a flow it
/// does not own, which needs either a reader-side way to stamp the timestamp
/// or the server declining precise latency measurement over a replicated
/// source. Until one of those lands, this is the containment.
///
/// One second, because it needs to outlast a producer hiccup and nothing else.
/// It is not a knob: the resume path reopens the writer on the next grain and
/// stamps it in the same iteration, so no grain loses its timestamp and there
/// is no behaviour here for an operator to tune (WRS §3).
constexpr auto LATENCY_WRITER_GRACE = std::chrono::seconds{1};

} // namespace
void Initiator::run(Config config, utils::ExitSignal sig) {
  Initiator{std::move(config)}.run(sig);
}

Initiator::Initiator(Config config)
    : _mxl(config.domain), _fabrics(_mxl), _config(config),
      _metrics(config.metricsSocket, false) {}

bool Initiator::measurePreciseNetworkLatency() const noexcept {
  return !_config.noNetworkLatencyMeasurement;
}

void Initiator::run(utils::ExitSignal sig) {
  auto flowVariant = _mxl.openFlow(_config.flowId);
  if (std::holds_alternative<::mxl::DiscreteFlowReader>(flowVariant)) {
    auto reader = std::get<::mxl::DiscreteFlowReader>(std::move(flowVariant));
    // The latency writer is opened by the transfer loop against the first
    // grain it reads, not here: holding it across a pause is what strands the
    // flow (see LATENCY_WRITER_GRACE).
    auto initiator = createInitiator(reader);
    auto targetInfo = ::mxl::fabrics::TargetInfo::parse(_config.targetInfo);
    initiator.addTarget(targetInfo);
    connect(initiator, sig);
    auto _ = ScopedRTScheduling{_config.schedPrio};
    transferGrains(std::move(reader), std::move(initiator), sig);
  } else {
    auto reader = std::get<::mxl::ContinuousFlowReader>(std::move(flowVariant));
    auto initiator = createInitiator(reader);
    auto targetInfo = ::mxl::fabrics::TargetInfo::parse(_config.targetInfo);
    initiator.addTarget(targetInfo);
    connect(initiator, sig);
    auto _ = ScopedRTScheduling{_config.schedPrio};
    transferSamples(std::move(reader), std::move(initiator), sig);
  }
}

::mxl::DiscreteFlowWriter
Initiator::openWriter(::mxl::DiscreteFlowReader const &reader) {
  auto writer = _mxl.createFlow(reader.getFlowDefinition());
  if (!std::holds_alternative<DiscreteFlowWriter>(writer)) {
    throw ::mxl::Exception{MXL_ERR_INVALID_ARG,
                           "request flow is not a discrete flow"};
  }

  return std::get<DiscreteFlowWriter>(std::move(writer));
}

::mxl::fabrics::DiscreteFlowInitiator
Initiator::createInitiator(DiscreteFlowReader &reader) {
  return _fabrics.createInitiator(reader,
                                  {
                                      .node = _config.node,
                                      .service = _config.service,
                                      .interface = _config.interfaceConfig(),
                                  });
}

::mxl::fabrics::ContinuousFlowInitiator
Initiator::createInitiator(::mxl::ContinuousFlowReader &reader) {
  return _fabrics.createInitiator(reader,
                                  {
                                      .node = _config.node,
                                      .service = _config.service,
                                      .interface = _config.interfaceConfig(),
                                  });
}

void Initiator::connect(::mxl::fabrics::DiscreteFlowInitiator &initiator,
                        utils::ExitSignal sig) {
  connectLoop(initiator, sig, _config.connectTimeout);
}

void Initiator::connect(::mxl::fabrics::ContinuousFlowInitiator &initiator,
                        utils::ExitSignal sig) {
  connectLoop(initiator, sig, _config.connectTimeout);
}

void Initiator::transferGrains(::mxl::DiscreteFlowReader reader,
                               ::mxl::fabrics::DiscreteFlowInitiator initiator,
                               utils::ExitSignal sig) {
  std::uint64_t index = 0;
  auto lastSuccessfullGrainRead = std::chrono::steady_clock::now();
  // Held only while grains are flowing, and reopened on the next one. See
  // LATENCY_WRITER_GRACE for why it may not outlive the grains.
  std::optional<::mxl::DiscreteFlowWriter> writer = std::nullopt;
  for (;;) {
    try {
      if (sig.shouldExit()) {
        return;
      }

      auto grainAccess = reader.getGrain(index, std::chrono::milliseconds(100));
      lastSuccessfullGrainRead = std::chrono::steady_clock::now();
      auto rxTime = ::mxlGetTime();
      if (measurePreciseNetworkLatency()) {
        // Reopened before the stamp rather than after it, so the grain that
        // ends a pause carries a timestamp like any other. `createFlow` is
        // create-or-open, so this attaches to the existing flow.
        if (!writer) {
          writer.emplace(openWriter(reader));
        }
        auto writeAccess = writer->openGrain(index);
        writeAccess.writeTxTimestamp(::mxlGetTime());
        writeAccess.cancel();
      }
      spdlog::debug("transmitting grain index={} fromSlice={} toSlice={}",
                    index, 0, grainAccess.validSlices());
      if (!initiator.transfer(index, 0, grainAccess.validSlices())) {
        spdlog::warn("grain at index={} discarded because initiator is "
                     "not ready",
                     index);
      }
      while (initiator.makeProgress(std::chrono::milliseconds(1000))) {
        spdlog::warn("grain still not transmitted after timeout");
        if (sig.shouldExit()) {
          return;
        }
      }

      // Calculate the number of skipped grains
      std::uint64_t skipped = 0;
      if (_lastIndex < index) {
        if (_lastIndex != 0) {
          skipped = index - (_lastIndex + 1);
        }

        _lastIndex = index;
      }

      auto rate = reader.getRate();
      auto grainSize = grainAccess.size();

      // Calculate the source latency
      auto grainTime = ::mxlIndexToTimestamp(&rate, index);
      auto sourceLatency = std::uint64_t{0};
      if (rxTime > grainTime) {
        sourceLatency = rxTime - grainTime;
      }

      _metrics.observe(grainSize, grainSize + 4096, 1, skipped, sourceLatency,
                       0);
      ++index;
    } catch (::mxl::Exception const &ex) {
      auto timeSinceLastGrainRead =
          std::chrono::steady_clock::now() - lastSuccessfullGrainRead;

      // Nothing is flowing, so the tx timestamp has nothing to stamp and the
      // lock the writer holds is pure cost. Releasing it here is what lets a
      // withdrawn target worker's flow be cleaned up rather than stranded
      // (LATENCY_WRITER_GRACE). Ahead of the idle-timeout check only for
      // tidiness: that path throws and unwinds the writer anyway.
      if (writer && (timeSinceLastGrainRead > LATENCY_WRITER_GRACE)) {
        spdlog::debug("releasing the latency writer after {}ms without a grain",
                      std::chrono::duration_cast<std::chrono::milliseconds>(
                          timeSinceLastGrainRead)
                          .count());
        writer.reset();
      }

      if (_config.idleTimeout &&
          (timeSinceLastGrainRead > *_config.idleTimeout)) {
        throw Exception{MXL_ERR_TIMEOUT,
                        "timed out waiting for a grain to be published "
                        "to the flow"};
      }

      if (ex.isTooEarly() || ex.isTooLate()) {
        index = reader.getHeadIndex() + 1;
        continue;
      }

      throw ex;
    }
  }
}

void Initiator::transferSamples(
    ::mxl::ContinuousFlowReader reader,
    ::mxl::fabrics::ContinuousFlowInitiator initiator, utils::ExitSignal sig) {
  auto lastReadTime = ::mxlGetTime();
  auto headIndex = reader.getHeadIndex();
  auto batchSize = reader.getBatchSize();
  auto rate = reader.getRate();
  auto interval =
      static_cast<std::uint64_t>(((static_cast<double>(rate.denominator) /
                                   static_cast<double>(rate.numerator)) *
                                  static_cast<double>(batchSize)) *
                                 std::nano::den);
  for (;;) {
    if (sig.shouldExit()) {
      return;
    }

    try {
      // Return value discarded: data is in shared memory; we only call
      // this to verify availability before the RDMA transfer.
      (void)reader.getSamplesNonBlocking(headIndex, batchSize);
    } catch (::mxl::Exception const &ex) {
      if (ex.isTooLate()) {
        headIndex = reader.getHeadIndex();
        continue;
      }
      if (ex.isTooEarly()) {
        continue;
      }
      throw;
    }
    lastReadTime = ::mxlGetTime();
    auto rxTime = lastReadTime;

    spdlog::debug("transferring samples headIndex={} count={}", headIndex,
                  batchSize);
    if (!initiator.transferSamples(headIndex, batchSize)) {
      spdlog::warn("samples at headIndex={} discarded because initiator "
                   "is not ready",
                   headIndex);
    }
    while (initiator.makeProgress(std::chrono::milliseconds(1000))) {
      if (sig.shouldExit()) {
        return;
      }
    }

    headIndex += batchSize;
    ::mxlSleepUntil(lastReadTime + interval);
  }
}

} // namespace mxl::proxy
