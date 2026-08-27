#pragma once

#include <cstdint>
#include <initializer_list>
#include <map>
#include <stdexcept>
#include <string>
#include <string_view>

/// A small long-option parser for the mock tools.
///
/// Deliberately not the worker's argument parser, which handles three flags and
/// says so. Deliberately not a dependency either: these tools are built by the
/// same CMake project as the worker and the container image builds that
/// project, so a new find_package here is a new package in the image for the
/// benefit of a test tool.
///
/// Long options only — `--name value` or `--name=value`, plus `--flag` — since
/// the callers are test scripts and Go test code, which spell things out.
/// Unknown options are an error rather than being ignored: a silently dropped
/// `--verfiy` in a test harness reports success for a check that never ran.
namespace mxl::mock {

/// Thrown for anything the caller got wrong. The tools print usage and exit
/// 2 on this, distinct from a runtime failure.
class UsageError : public std::runtime_error {
  public:
    using std::runtime_error::runtime_error;
};

class Args {
  public:
    /// Parses argv, or throws [UsageError]. `known` is every option the
    /// tool accepts, without the leading dashes.
    static Args parse(int argc, char** argv,
                      std::initializer_list<std::string_view> known);

    [[nodiscard]] bool flag(std::string_view name) const;
    [[nodiscard]] bool has(std::string_view name) const;

    /// The value of an option, or `fallback` when it was not given.
    [[nodiscard]] std::string str(std::string_view name,
                                  std::string_view fallback = {}) const;

    /// The value of an option, or [UsageError] when it was not given.
    [[nodiscard]] std::string required(std::string_view name) const;

    /// An unsigned value, or `fallback`. Throws [UsageError] on garbage
    /// rather than silently yielding zero, which for `--count` would mean
    /// "run forever".
    [[nodiscard]] std::uint64_t num(std::string_view name,
                                    std::uint64_t fallback) const;

  private:
    std::map<std::string, std::string, std::less<>> _values;
};

} // namespace mxl::mock
