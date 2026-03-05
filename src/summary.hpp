#pragma once

#include <array>
#include <chrono>
#include <cstdint>
#include <string>
#include <vector>

namespace mxl::proxy {

struct Counter {
    std::string name;
    std::uint64_t value;

    void add(std::uint64_t v) noexcept;
};

struct Quantile {
    Quantile(double q, double error);

    double quantile;
    double error;
    double u;
    double v;
};

struct QuantileBucket {
    QuantileBucket(std::initializer_list<double> qs);

    std::string name;
    std::vector<Quantile> quantiles;
    void observe(double v) noexcept;
    double get(double q);
    void reset();

  private:
    struct Item {
        double value;
        int g;
        int delta;
    };

  private:
    double errorBound(int rank);
    bool batch();
    void compress();

  private:
    std::vector<Quantile> _quantiles;
    std::vector<Item> _items = {};
    std::array<double, 512> _buffer = {};
    std::size_t _bufferIndex = 0;
    std::size_t _count = 0;
};

class Summary {
  public:
    Summary(std::string name, std::chrono::steady_clock::duration maxAge,
            std::size_t buckets, std::initializer_list<double> qs);

    void observe(double v) noexcept;

    double get(double q);

  private:
    friend std::ostream& operator<<(std::ostream& stream, Summary const&);

  private:
    QuantileBucket& rotate();

    std::string _name;
    std::chrono::steady_clock::duration _rotationInterval;
    std::chrono::steady_clock::time_point _lastRotation;
    std::size_t _currentBucket;
    std::vector<double> _quantiles;
    std::vector<QuantileBucket> _buckets;
};

Summary makeSummary(std::string_view name, std::initializer_list<double> qs);
Counter makeCounter(std::string_view name);

std::ostream& operator<<(std::ostream& stream, Counter const&);
std::ostream& operator<<(std::ostream& stream, Summary const&);
} // namespace mxl::proxy
