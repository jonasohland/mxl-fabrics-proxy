#pragma once

#include <cstddef>
#include <cstdint>

/// Deterministic payload patterns shared by mxl-mock-src and mxl-mock-sink.
///
/// The point of writing a pattern rather than zeroes is that it turns an
/// end-to-end test from "the head index advanced" into "the right bytes arrived
/// at the right offsets". That distinction is the whole reason these tools
/// exist: a fabrics session can move data and still be wrong, because the
/// initiator computes scatter-gather offsets *within* the bounce buffer ring
/// from entrySize/entryCount, and a stale value puts writes at the wrong
/// offsets inside a correctly registered memory region. The NIC sees nothing
/// wrong, the target unpacks garbage, and every counter in the system reports
/// healthy. Zeroed payloads cannot catch that; a per-index pattern can.
///
/// The pattern is a pure function of (seed, index) with no state carried
/// between grains, so a reader can join a flow at any point and verify from
/// there, and a grain that arrives at the wrong index fails just as loudly as
/// one whose bytes were corrupted.
namespace mxl::mock {

/// splitmix64. Fast, well-distributed, and short enough to reimplement in
/// another language if a test ever needs to.
std::uint64_t mix(std::uint64_t value) noexcept;

/// Fill a discrete grain's payload with the pattern for `index`.
void fillGrain(std::uint8_t* payload, std::size_t size, std::uint64_t seed,
               std::uint64_t index) noexcept;

/// Verify a discrete grain's payload. Returns the byte offset of the first
/// mismatch, or `size` when the whole grain matches.
std::size_t verifyGrain(std::uint8_t const* payload, std::size_t size,
                        std::uint64_t seed, std::uint64_t index) noexcept;

/// The value for one sample of a continuous flow.
///
/// Keyed by channel and *absolute sample index* rather than by batch, so a
/// reader batching differently from the writer still verifies — which
/// matters, because the batch size is a property of the flow and the
/// transfer path is free to regroup.
std::uint64_t sampleValue(std::uint64_t seed, std::uint64_t channel,
                          std::uint64_t sampleIndex) noexcept;

/// Write `sampleValue` over one sample of `size` bytes, truncating or
/// repeating the 64-bit value as needed.
void fillSample(std::uint8_t* sample, std::size_t size,
                std::uint64_t value) noexcept;

/// Whether a sample of `size` bytes carries `value`.
bool verifySample(std::uint8_t const* sample, std::size_t size,
                  std::uint64_t value) noexcept;

} // namespace mxl::mock
