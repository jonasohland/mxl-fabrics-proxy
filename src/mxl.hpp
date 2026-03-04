#pragma once

#include <chrono>
#include <memory>
#include <mxl/flow.h>
#include <mxl/mxl.h>
#include <string>
#include <string_view>
#include <variant>

namespace mxl {

namespace fabrics {
class Instance;
class DiscreteFlowInitiator;
class DiscreteFlowTarget;
class ContinuousFlowInitiator;
class ContinuousFlowTarget;
} // namespace fabrics

class Instance;
class ContinuousFlowWriter;
class ContinuousFlowReader;
class DiscreteFlowReader;
class DiscreteFlowWriter;

class Exception : public std::exception {
  public:
    Exception(::mxlStatus status, char const* msg);
    Exception(::mxlStatus status, std::string msg);

    [[nodiscard]] char const* what() const noexcept override;

    [[nodiscard]] ::mxlStatus status() const noexcept;
    [[nodiscard]] bool isOutOfRange() const noexcept;
    [[nodiscard]] bool isTooEarly() const noexcept;
    [[nodiscard]] bool isTooLate() const noexcept;
    [[nodiscard]] bool isFlowInvalid() const noexcept;
    [[nodiscard]] bool isInterrupted() const noexcept;
    [[nodiscard]] bool isNotReady() const noexcept;
    [[nodiscard]] bool isTimeout() const noexcept;

  private:
    void ensureFormatted() const noexcept;

    ::mxlStatus _status;
    char const* _msg;
    mutable std::string _formatted;
};

std::string_view mxlStatusName(::mxlStatus status);

template <typename F, typename... Args>
void mxl(F&& f, char const* message, Args&&... args) {
    auto const ret = f(std::forward<Args>(args)...);
    if (ret != MXL_STATUS_OK) {
        throw Exception(ret, message);
    }
}

class Instance {
  private:
    friend class fabrics::Instance;
    friend class DiscreteFlowReader;
    friend class DiscreteFlowWriter;
    friend class ContinuousFlowReader;
    friend class ContinuousFlowWriter;

  private:
    struct Inner {
        explicit Inner(std::string const& domainPath);
        ~Inner();
        Inner(Inner&&) = delete;
        Inner(Inner const&) = delete;
        Inner& operator=(Inner&&) = delete;
        Inner& operator=(Inner const&) = delete;
        ::mxlInstance instance;
    };

  public:
    explicit Instance(std::string const& domainPath);
    ~Instance() = default;

    [[nodiscard]] std::variant<DiscreteFlowReader, ContinuousFlowReader>
    openFlow(std::string const& id) const;
    [[nodiscard]] std::variant<DiscreteFlowWriter, ContinuousFlowWriter>
    createFlow(std::string const& flowDesc) const;

  private:
    ::mxlInstance raw() noexcept;

    void destroy(::mxlFlowReader) const;
    void destroy(::mxlFlowWriter) const;

  private:
    std::shared_ptr<Inner> _inner;
};

class DiscreteFlowReader {
  private:
    friend class Instance;
    friend class fabrics::Instance;

  public:
    DiscreteFlowReader(DiscreteFlowReader&&);
    ~DiscreteFlowReader();

    class Access {
      public:
        /** Get the total payload size. */
        [[nodiscard]] std::size_t size() const noexcept;

        /** Get a pointer to the payload data. */
        [[nodiscard]] std::uint8_t const* data() const noexcept;

        /** Get the total number of slices in the grain. */
        [[nodiscard]] std::uint16_t totalSlices() const noexcept;

        /** Get the number of valid payload slices in the grain. */
        [[nodiscard]] std::uint16_t validSlices() const noexcept;

      private:
        Access(mxlGrainInfo, std::uint8_t const* payload);

      private:
        friend class DiscreteFlowReader;

      private:
        ::mxlGrainInfo _info;
        std::uint8_t const* _payload;
    };

    /** Get the current head index of the flow */
    [[nodiscard]] std::uint64_t getHeadIndex() const;

    /** Get a grain. Blocks until the full grain (all slices) is available. */
    [[nodiscard]] Access getGrain(std::uint64_t index,
                                  std::chrono::nanoseconds timeout);

    /** Get a grain without blocking. Throws when the requested grain is not
     * available */
    [[nodiscard]] Access getGrainNonBlocking(std::uint64_t index);

    /** Get a grain at the specified index where at least `minValidSlices`
     * number of slices are valid */
    [[nodiscard]] Access getGrainSlices(std::uint64_t index,
                                        std::uint16_t minValidSlices,
                                        std::chrono::nanoseconds timeout);

    /** Get a grain at the specified index where at least `minValidSlices`
     * number of slices are valid */
    [[nodiscard]] Access
    getGrainSlicesNonBlocking(std::uint64_t index,
                              std::uint16_t minValidSlices);

  private:
    DiscreteFlowReader(::mxlFlowReader, ::mxlFlowConfigInfo, Instance);
    ::mxlFlowReader raw();

  private:
    ::mxlFlowReader _reader;
    ::mxlFlowConfigInfo _configInfo;
    Instance _instance;
};

class ContinuousFlowReader {
  public:
    ~ContinuousFlowReader();

  private:
    ContinuousFlowReader(::mxlFlowReader, ::mxlFlowConfigInfo, Instance);

  private:
    friend class Instance;
    ::mxlFlowReader _reader;
    ::mxlFlowConfigInfo _configInfo;
    Instance _instance;
};

class DiscreteFlowWriter {
  public:
    DiscreteFlowWriter(DiscreteFlowWriter&&);
    ~DiscreteFlowWriter();

    class Access {
      public:
        Access(const Access&) = default;
        Access(Access&&) = delete;
        Access& operator=(const Access&) = delete;
        Access& operator=(Access&&) = delete;
        /** When the access structure is destroyed, the changes are committed */
        ~Access() noexcept(false);

        /** Cancel the write operation */
        void cancel();

        /** Get the grain index used by this ring buffer entry */
        [[nodiscard]] std::uint64_t index() const noexcept;

        /** Get the total accessible payload size */
        [[nodiscard]] std::size_t size() const noexcept;

        /** Get a pointer to the beginning of the payload */
        [[nodiscard]] std::uint8_t const* data() const noexcept;

        /** Get a pointer to the beginning of the payload */
        [[nodiscard]] std::uint8_t* data() noexcept;

        /** Get the total number of slices in the payload */
        [[nodiscard]] std::uint16_t totalSlices() const noexcept;

        /** Get the number of slices of the payload that are valid */
        [[nodiscard]] std::uint16_t validSlices() const noexcept;

        /** Set the number of slices of the payload that are valid */
        void validSlices(std::uint16_t validSlices) noexcept;

      private:
        friend class DiscreteFlowWriter;

      private:
        Access(DiscreteFlowWriter&, ::mxlGrainInfo, std::uint8_t*);

      private:
        DiscreteFlowWriter& _writer;
        ::mxlGrainInfo _info;
        std::uint8_t* _payload;
    };

    [[nodiscard]] Access openGrain(std::uint64_t index);

  private:
    friend class Instance;
    friend class Access;
    friend class fabrics::Instance;

  private:
    DiscreteFlowWriter(::mxlFlowWriter, ::mxlFlowConfigInfo, Instance);
    ::mxlFlowWriter raw();
    void cancel();
    void commit(::mxlGrainInfo const& info);

  private:
    ::mxlFlowWriter _writer;
    ::mxlFlowConfigInfo _info;
    Instance _instance;
};

class ContinuousFlowWriter {};

} // namespace mxl
