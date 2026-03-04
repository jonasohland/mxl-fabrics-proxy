#pragma once

#include "mxl.hpp"
#include <memory>
#include <mxl/fabrics.h>

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

  public:
    explicit Instance(mxl::Instance);

    std::pair<DiscreteFlowTarget, TargetInfo> createTarget(DiscreteFlowWriter&,
                                                           TargetConfig);
    DiscreteFlowInitiator createInitiator(DiscreteFlowReader&, InitiatorConfig);

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

} // namespace mxl::fabrics
