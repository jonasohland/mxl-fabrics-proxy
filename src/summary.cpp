#include "summary.hpp"
#include <algorithm>
#include <cmath>

namespace mxl::proxy {
Counter makeCounter(std::string_view name) {
    return Counter{.name = std::string(name), .value = 0};
}

Summary makeSummary(std::string_view name, std::initializer_list<double> qs) {
    return Summary{std::string{name}, std::chrono::seconds(30), 3,
                   std::move(qs)};
}

void Counter::add(std::uint64_t v) noexcept {
    value += v;
}

Quantile::Quantile(double q, double error)
    : quantile(q),
      error(error),
      u(2 * error / (1 - quantile)),
      v(2 * error / quantile) {
}

QuantileBucket::QuantileBucket(std::initializer_list<double> qs) {
    for (auto q : qs) {
        _quantiles.emplace_back(q, 0.05);
    }
}

void QuantileBucket::observe(double v) noexcept {
    _buffer[_bufferIndex] = v;
    ++_bufferIndex;
    if (_bufferIndex == _buffer.size()) {
        batch();
        compress();
    }
}

double QuantileBucket::get(double q) {
    batch();
    compress();

    if (_items.empty()) {
        return std::numeric_limits<double>::quiet_NaN();
    }

    int rankMin = 0;
    const auto desired = static_cast<int>(q * _count);
    const auto bound = desired + (errorBound(desired) / 2);

    auto it = _items.begin();
    decltype(it) prev;
    auto cur = it++;

    while (it != _items.end()) {
        prev = cur;
        cur = it++;

        rankMin += prev->g;

        if (rankMin + cur->g + cur->delta > bound) {
            return prev->value;
        }
    }

    return _items.back().value;
}

void QuantileBucket::reset() {
    _count = 0;
    _bufferIndex = 0;
    _items.clear();
}

double QuantileBucket::errorBound(int rank) {
    auto size = _items.size();
    double minError = size + 1;

    for (const auto& q : _quantiles) {
        double error;
        if (rank <= q.quantile * size) {
            error = q.u * (size - rank);
        } else {
            error = q.v * rank;
        }
        if (error < minError) {
            minError = error;
        }
    }

    return minError;
}

bool QuantileBucket::batch() {
    if (_bufferIndex == 0) {
        return false;
    }

    std::sort(_buffer.begin(), _buffer.begin() + _bufferIndex);

    std::size_t start = 0;
    if (_items.empty()) {
        _items.emplace_back(_buffer[0], 1, 0);
        ++start;
        ++_count;
    }

    std::size_t idx = 0;
    std::size_t item = idx++;

    for (std::size_t i = start; i < _bufferIndex; ++i) {
        double v = _buffer[i];
        while (idx < _items.size() && _items[item].value < v) {
            item = idx++;
        }

        if (_items[item].value > v) {
            --idx;
        }

        int delta;
        if (idx - 1 == 0 || idx + 1 == _items.size()) {
            delta = 0;
        } else {
            delta = static_cast<int>(std::floor(errorBound(idx + 1))) + 1;
        }

        _items.emplace(_items.begin() + idx, v, 1, delta);
        _count++;
        item = idx++;
    }

    _bufferIndex = 0;
    return true;
}

void QuantileBucket::compress() {
    if (_items.size() < 2) {
        return;
    }

    std::size_t idx = 0;
    std::size_t prev;
    std::size_t next = idx++;

    while (idx < _items.size()) {
        prev = next;
        next = idx++;

        if (_items[prev].g + _items[next].g + _items[next].delta <=
            errorBound(idx - 1)) {
            _items[next].g += _items[prev].g;
            _items.erase(_items.begin() + prev);
        }
    }
}

Summary::Summary(std::string name, std::chrono::steady_clock::duration maxAge,
                 std::size_t buckets, std::initializer_list<double> qs)
    : _name(std::move(name)),
      _rotationInterval(maxAge / buckets),
      _lastRotation(std::chrono::steady_clock::now()),
      _currentBucket(0),
      _quantiles(qs),
      _buckets(buckets, QuantileBucket{qs})

{
}

void Summary::observe(double v) noexcept {
    rotate();
    for (auto& bucket : _buckets) {
        bucket.observe(v);
    }
}

double Summary::get(double q) {
    return rotate().get(q);
}

QuantileBucket& Summary::rotate() {
    auto delta = std::chrono::steady_clock::now() - _lastRotation;
    while (delta > _rotationInterval) {
        _buckets[_currentBucket].reset();

        if (++_currentBucket >= _buckets.size()) {
            _currentBucket = 0;
        }

        delta -= _rotationInterval;
        _lastRotation += _rotationInterval;
    }
    return _buckets[_currentBucket];
}

std::ostream& operator<<(std::ostream& stream, Counter const& counter) {
    stream << counter.name << " " << counter.value << "\n";
    return stream;
}

std::ostream& operator<<(std::ostream& stream, Summary const& summary) {
    auto s = const_cast<Summary&>(summary);
    for (auto const& quant : summary._quantiles) {
        stream << summary._name << "[" << quant << "] " << s.get(quant) << "\n";
    }

    return stream;
}
} // namespace mxl::proxy
