// SPDX-FileCopyrightText: 2025 Contributors to the Media eXchange Layer
// project. SPDX-License-Identifier: Apache-2.0

#pragma once

#include <utility>

namespace mxl {
/** \brief Helper class that is created by a call to \see defer().
 * When a object created from this class goes out of scope, the function passed
 * to defer() is run. This class should never be used directly.
 */
template <typename F> class Deferred {
  public:
    Deferred(Deferred<F> const&) = delete;
    Deferred(Deferred<F>&&) = delete;
    Deferred& operator=(Deferred<F> const&) = delete;
    Deferred& operator=(Deferred<F>&&) = delete;

    /** \brief Calls the deferred function
     */
    constexpr ~Deferred() noexcept(std::is_nothrow_invocable_v<F>) {
        _f();
    }

  private:
    /** \brief The construtor is private, an can only be called from defer()
     */
    constexpr Deferred(F&& f) : _f(std::forward<F>(f)) {
    }

    /** \brief Allow Deferred to be constructed from defer()
     */
    template <typename N> friend constexpr Deferred<N> defer(N&& f) noexcept;

  private:
    F _f;
};

template <typename F>
[[nodiscard("The value returned from defer() must not be discarded. Discarding "
            "it calls the function passed to defer() right away.")]]
constexpr Deferred<F> defer(F&& f) noexcept {
    return Deferred{std::forward<F>(f)};
}
} // namespace mxl
