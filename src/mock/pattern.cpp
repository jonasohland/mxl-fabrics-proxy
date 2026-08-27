#include "pattern.hpp"

#include <cstring>

namespace mxl::mock {

std::uint64_t mix(std::uint64_t value) noexcept {
    value += 0x9e3779b97f4a7c15ULL;
    value = (value ^ (value >> 30)) * 0xbf58476d1ce4e5b9ULL;
    value = (value ^ (value >> 27)) * 0x94d049bb133111ebULL;
    return value ^ (value >> 31);
}

namespace {

/// The word at `word` of the grain at `index`. Position-dependent on
/// purpose: a grain shifted by even one word fails verification, which
/// is exactly the bounce-buffer-offset failure these tools exist to
/// catch.
std::uint64_t grainWord(std::uint64_t seed, std::uint64_t index,
                        std::uint64_t word) noexcept {
    return mix(mix(seed ^ (index * 0x100000001b3ULL)) ^ word);
}

} // namespace

void fillGrain(std::uint8_t* payload, std::size_t size, std::uint64_t seed,
               std::uint64_t index) noexcept {
    std::size_t offset = 0;
    std::uint64_t word = 0;
    for (; offset + sizeof(std::uint64_t) <= size;
         offset += sizeof(std::uint64_t), ++word) {
        auto const value = grainWord(seed, index, word);
        std::memcpy(payload + offset, &value, sizeof(value));
    }
    if (offset < size) {
        auto const value = grainWord(seed, index, word);
        std::memcpy(payload + offset, &value, size - offset);
    }
}

std::size_t verifyGrain(std::uint8_t const* payload, std::size_t size,
                        std::uint64_t seed, std::uint64_t index) noexcept {
    std::size_t offset = 0;
    std::uint64_t word = 0;
    for (; offset + sizeof(std::uint64_t) <= size;
         offset += sizeof(std::uint64_t), ++word) {
        auto const expected = grainWord(seed, index, word);
        std::uint64_t actual = 0;
        std::memcpy(&actual, payload + offset, sizeof(actual));
        if (actual != expected) {
            return offset;
        }
    }
    if (offset < size) {
        auto const expected = grainWord(seed, index, word);
        if (std::memcmp(payload + offset, &expected, size - offset) != 0) {
            return offset;
        }
    }
    return size;
}

std::uint64_t sampleValue(std::uint64_t seed, std::uint64_t channel,
                          std::uint64_t sampleIndex) noexcept {
    return mix(mix(seed ^ (channel << 32)) ^ sampleIndex);
}

void fillSample(std::uint8_t* sample, std::size_t size,
                std::uint64_t value) noexcept {
    std::size_t offset = 0;
    for (; offset + sizeof(value) <= size; offset += sizeof(value)) {
        std::memcpy(sample + offset, &value, sizeof(value));
    }
    if (offset < size) {
        std::memcpy(sample + offset, &value, size - offset);
    }
}

bool verifySample(std::uint8_t const* sample, std::size_t size,
                  std::uint64_t value) noexcept {
    std::size_t offset = 0;
    for (; offset + sizeof(value) <= size; offset += sizeof(value)) {
        if (std::memcmp(sample + offset, &value, sizeof(value)) != 0) {
            return false;
        }
    }
    if (offset < size) {
        return std::memcmp(sample + offset, &value, size - offset) == 0;
    }
    return true;
}

} // namespace mxl::mock
