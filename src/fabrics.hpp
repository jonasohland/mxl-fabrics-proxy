#pragma once

#include "mxl.hpp"
#include <cstdint>
#include <memory>
#include <mxl/fabrics.h>
#include <optional>
#include <string_view>
#include <utility>
#include <vector>

namespace mxl::fabrics {

class DiscreteFlowTarget;
class DiscreteFlowInitiator;
class ContinuousFlowTarget;
class ContinuousFlowInitiator;

/** The endpoint configuration both ends of a session must agree on. The library
 * does no negotiation of its own: it is documented as requiring the target and
 * the initiator to be handed the same capabilities and maximum message size,
 * with the caller's out-of-band channel responsible for agreeing them.
 */
struct InterfaceConfig {
    ::mxlFabricsProvider provider;
    std::uint64_t capsFlags;
    std::uint64_t maxMessageSize;
};

struct TargetConfig {
    std::string node;
    std::string service;
    InterfaceConfig interface;
};

struct InitiatorConfig {
    std::string node;
    std::string service;
    InterfaceConfig interface;
};

/** One (interface, address, provider) combination available on this host, as
 * reported by mxlFabricsGetInterfaces(). The same physical interface appears
 * several times when it is reachable through several providers or carries
 * several addresses.
 */
struct InterfaceInfo {
    ::mxlFabricsProvider provider;
    std::string node;
    std::string service;
    std::uint64_t capsFlags;
    std::uint64_t maxMessageSize;

    /** Best-effort extra information as a JSON object, verbatim from the
     * library. Empty when it reported none. Contents vary by platform and
     * hardware; `device_name` is the interesting one and is not always present.
     */
    std::string attr;
};

/** Provider name, as used in the worker config and in the --interfaces output.
 */
[[nodiscard]] std::string_view providerName(::mxlFabricsProvider) noexcept;

/** Capability flag names for a bitmask, in a stable order. */
[[nodiscard]] std::vector<std::string> capsFlagNames(std::uint64_t flags);

/** The bit for one capability flag name. Throws MXL_ERR_INVALID_ARG on an
 * unknown name — a caps set the operator meant to constrain must never be
 * silently widened by a typo.
 */
[[nodiscard]] std::uint64_t capsFlagFromName(std::string_view name);

class TargetInfo {
  public:
    ~TargetInfo();

    TargetInfo(TargetInfo&&);

    static TargetInfo parse(std::string const&);
    [[nodiscard]] std::string id() const noexcept;
    [[nodiscard]] std::string toString() const;

  private:
    TargetInfo(std::string, ::mxlFabricsTargetInfo);
    TargetInfo(::mxlFabricsTargetInfo);

    [[nodiscard]] ::mxlFabricsTargetInfo raw() const;

  private:
    friend class DiscreteFlowTarget;
    friend class DiscreteFlowInitiator;
    friend class ContinuousFlowTarget;
    friend class ContinuousFlowInitiator;

  private:
    std::string _id;
    ::mxlFabricsTargetInfo _targetInfo;
};

class Instance {
  private:
    struct Inner {
        Inner(Inner&&) = delete;
        Inner(Inner const&) = delete;
        Inner& operator=(Inner&&) = delete;
        Inner& operator=(Inner const&) = delete;

        explicit Inner(mxl::Instance);
        ~Inner();

        mxl::Instance mxlInstance;
        ::mxlFabricsInstance instance;
    };

  private:
    friend class DiscreteFlowTarget;
    friend class DiscreteFlowInitiator;
    friend class ContinuousFlowTarget;
    friend class ContinuousFlowInitiator;

  public:
    explicit Instance(mxl::Instance);

    /** Enumerate every interface libfabric offers on this host. The domain
     * behind the mxl instance is not used; only the fabrics instance is.
     */
    [[nodiscard]] std::vector<InterfaceInfo> getInterfaces() const;

    std::pair<DiscreteFlowTarget, TargetInfo>
    createTarget(mxl::DiscreteFlowWriter&, TargetConfig);

    std::pair<ContinuousFlowTarget, TargetInfo>
    createTarget(mxl::ContinuousFlowWriter&, TargetConfig);

    DiscreteFlowInitiator createInitiator(mxl::DiscreteFlowReader&,
                                          InitiatorConfig);

    ContinuousFlowInitiator createInitiator(mxl::ContinuousFlowReader&,
                                            InitiatorConfig);

  private:
    void destroy(::mxlFabricsTarget);
    void destroy(::mxlFabricsInitiator);

  private:
    std::shared_ptr<Inner> _inner;
};

class DiscreteFlowTarget {
  private:
    friend class Instance;

  public:
    ~DiscreteFlowTarget();
    DiscreteFlowTarget(DiscreteFlowTarget const&) = delete;
    DiscreteFlowTarget(DiscreteFlowTarget&&);

    std::optional<std::uint64_t> readGrain(std::chrono::milliseconds);
    std::optional<std::uint64_t> readGrainNonBlocking();

  private:
    DiscreteFlowTarget(Instance, ::mxlFabricsTarget);
    TargetInfo setup(::mxlFabricsTargetConfig const&);

  private:
    Instance _instance;
    ::mxlFabricsTarget _target;
};

class DiscreteFlowInitiator {
  private:
    friend class Instance;

  public:
    ~DiscreteFlowInitiator();
    DiscreteFlowInitiator(DiscreteFlowInitiator const&) = delete;
    DiscreteFlowInitiator(DiscreteFlowInitiator&&);

    void addTarget(TargetInfo const& targetInfo);
    void removeTarget(TargetInfo const& targetInfo);

    bool transfer(std::uint64_t index, std::uint16_t fromSlice,
                  std::uint16_t toSlice);
    bool makeProgress(std::chrono::milliseconds timeout);
    bool makeProgressNonBlocking();

  private:
    DiscreteFlowInitiator(Instance, ::mxlFabricsInitiator);
    void setup(::mxlFabricsInitiatorConfig const&);

  private:
    Instance _instance;
    ::mxlFabricsInitiator _initiator;
};

class ContinuousFlowTarget {
  private:
    friend class Instance;

  public:
    ~ContinuousFlowTarget();
    ContinuousFlowTarget(ContinuousFlowTarget const&) = delete;
    ContinuousFlowTarget(ContinuousFlowTarget&&);

    std::optional<std::pair<std::uint64_t, std::size_t>>
    readSamples(std::chrono::milliseconds timeout);

    std::optional<std::pair<std::uint64_t, std::size_t>>
    readSamplesNonBlocking();

  private:
    ContinuousFlowTarget(Instance, ::mxlFabricsTarget);
    TargetInfo setup(::mxlFabricsTargetConfig const&);

  private:
    Instance _instance;
    ::mxlFabricsTarget _target;
};

class ContinuousFlowInitiator {
  private:
    friend class Instance;

  public:
    ~ContinuousFlowInitiator();
    ContinuousFlowInitiator(ContinuousFlowInitiator const&) = delete;
    ContinuousFlowInitiator(ContinuousFlowInitiator&&);

    void addTarget(TargetInfo const& targetInfo);
    void removeTarget(TargetInfo const& targetInfo);

    bool transferSamples(std::uint64_t headIndex, std::size_t count);
    bool makeProgress(std::chrono::milliseconds timeout);
    bool makeProgressNonBlocking();

  private:
    ContinuousFlowInitiator(Instance, ::mxlFabricsInitiator);
    void setup(::mxlFabricsInitiatorConfig const&);

  private:
    Instance _instance;
    ::mxlFabricsInitiator _initiator;
};

} // namespace mxl::fabrics
