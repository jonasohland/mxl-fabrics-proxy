#pragma once

#include "mxl.hpp"
#include <memory>
#include <mxl/fabrics.h>
#include <optional>
#include <utility>

namespace mxl::fabrics {

class DiscreteFlowTarget;
class DiscreteFlowInitiator;
class ContinuousFlowTarget;
class ContinuousFlowInitiator;

struct TargetConfig {
    std::string node;
    std::string service;
    ::mxlFabricsProvider provider;
};

struct InitiatorConfig {
    std::string node;
    std::string service;
    ::mxlFabricsProvider provider;
};

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

    std::pair<DiscreteFlowTarget, TargetInfo>
    createTarget(mxl::DiscreteFlowWriter&, TargetConfig);

    std::pair<ContinuousFlowTarget, TargetInfo>
    createTarget(mxl::ContinuousFlowWriter&, TargetConfig);

    DiscreteFlowInitiator
    createInitiator(mxl::DiscreteFlowReader&, InitiatorConfig);

    ContinuousFlowInitiator
    createInitiator(mxl::ContinuousFlowReader&, InitiatorConfig);

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
