#include "args.hpp"

#include <algorithm>
#include <charconv>
#include <format>
#include <vector>

namespace mxl::mock {

namespace {

bool isKnown(std::initializer_list<std::string_view> known,
             std::string_view name) {
    return std::find(known.begin(), known.end(), name) != known.end();
}

/// Options that never take a value. Everything else is `--name value`.
bool isFlag(std::string_view name) {
    return name == "help" || name == "verbose" || name == "json" ||
           name == "verify" || name == "zero";
}

} // namespace

Args Args::parse(int argc, char** argv,
                 std::initializer_list<std::string_view> known) {
    Args args;

    for (int i = 1; i < argc; ++i) {
        auto const arg = std::string_view{argv[i]};
        if (arg == "-h") {
            args._values.emplace("help", "");
            continue;
        }
        if (!arg.starts_with("--")) {
            throw UsageError{std::format("unexpected argument: {}", arg)};
        }

        auto body = arg.substr(2);
        auto value = std::string{};
        auto haveValue = false;

        if (auto const eq = body.find('='); eq != std::string_view::npos) {
            value = std::string{body.substr(eq + 1)};
            body = body.substr(0, eq);
            haveValue = true;
        }

        auto const name = std::string{body};
        if (!isKnown(known, name)) {
            throw UsageError{std::format("unknown option: --{}", name)};
        }

        if (isFlag(name)) {
            if (haveValue) {
                throw UsageError{std::format("--{} takes no value", name)};
            }
            args._values.emplace(name, "");
            continue;
        }

        if (!haveValue) {
            if (i + 1 >= argc) {
                throw UsageError{std::format("--{} needs a value", name)};
            }
            value = argv[++i];
        }
        args._values.insert_or_assign(name, std::move(value));
    }

    return args;
}

bool Args::flag(std::string_view name) const {
    return _values.contains(name);
}

bool Args::has(std::string_view name) const {
    return _values.contains(name);
}

std::string Args::str(std::string_view name, std::string_view fallback) const {
    auto const it = _values.find(name);
    return it == _values.end() ? std::string{fallback} : it->second;
}

std::string Args::required(std::string_view name) const {
    auto const it = _values.find(name);
    if (it == _values.end() || it->second.empty()) {
        throw UsageError{std::format("--{} is required", name)};
    }
    return it->second;
}

std::uint64_t Args::num(std::string_view name, std::uint64_t fallback) const {
    auto const it = _values.find(name);
    if (it == _values.end()) {
        return fallback;
    }
    auto const& text = it->second;
    std::uint64_t value = 0;
    auto const* const begin = text.data();
    auto const* const end = begin + text.size();
    auto const [ptr, ec] = std::from_chars(begin, end, value);
    if (ec != std::errc{} || ptr != end) {
        throw UsageError{
            std::format("--{} expects a number, got {}", name, text)};
    }
    return value;
}

} // namespace mxl::mock
