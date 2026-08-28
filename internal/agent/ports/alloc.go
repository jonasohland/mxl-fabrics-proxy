package ports

import (
	"fmt"
	"net"
	"slices"
	"strconv"
	"sync"

	"github.com/jonasohland/mxl-replicator/internal/api"
)

// Allocator hands out fabric endpoint services from a configured range, one per owner.
//
// An *owner* is a worker the agent supervises, identified by its session and role. Keying on the
// owner rather than on the call is what gives §7.4's port stability for free: a worker that is
// restarted — for a new epoch, after a crash, after its backoff — asks again and gets the same
// number back, so nothing downstream sees a port move for a reason that was not a port problem.
// The number is only returned to the pool when the session itself goes away.
//
// It explicitly does not carry forward the legacy supervisor's `rand.Intn(20000) + 20000`, which
// had no collision detection and no retry: a collision produced a bind failure and a restart loop
// that eventually rolled a different number.
//
// Safe for concurrent use, though in practice one reconcile goroutine owns it.
type Allocator struct {
	rng Range

	// listen is net.Listen, replaced in tests. It is the probe-bind, not a real listener: the
	// socket is closed immediately and the worker binds the port for itself.
	listen func(network, address string) (net.Listener, error)

	mu     sync.Mutex
	byName map[string]uint16
	inUse  map[uint16]string
	cursor uint16
}

// NewAllocator returns an allocator over an inclusive port range.
func NewAllocator(rng Range) (*Allocator, error) {
	if rng.IsZero() {
		return nil, fmt.Errorf("ports: no range configured")
	}
	if rng.Count() <= 0 {
		return nil, fmt.Errorf("ports: range %s is empty", rng)
	}
	return &Allocator{
		rng:    rng,
		listen: net.Listen,
		byName: map[string]uint16{},
		inUse:  map[uint16]string{},
		cursor: rng.Low,
	}, nil
}

// Range returns the configured range.
func (a *Allocator) Range() Range { return a.rng }

// Allocate returns the service for an owner, allocating one on first ask.
//
// The returned value is the worker config's `service` key (WRS §3): a port number for tcp, verbs
// and efa, and for shm not a port at all but a host-wide unique endpoint name — which is what a
// range allocator produces anyway. One allocator, one collision domain, and no per-provider
// branch in the agent (M0 plan decision).
func (a *Allocator) Allocate(owner string, provider api.Provider) (string, error) {
	if owner == "" {
		return "", fmt.Errorf("ports: allocate: no owner")
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	if port, ok := a.byName[owner]; ok {
		return strconv.FormatUint(uint64(port), 10), nil
	}

	probe := shouldProbe(provider)
	for range a.rng.Count() {
		port := a.cursor
		a.advanceLocked()

		if _, taken := a.inUse[port]; taken {
			continue
		}
		if probe && !a.freeLocked(port) {
			continue
		}

		a.byName[owner] = port
		a.inUse[port] = owner
		return strconv.FormatUint(uint64(port), 10), nil
	}

	return "", fmt.Errorf("ports: no free port in %s for %s, %d of %d are in use",
		a.rng, owner, len(a.inUse), a.rng.Count())
}

// Service returns the service already allocated to an owner, if any.
func (a *Allocator) Service(owner string) (string, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()

	port, ok := a.byName[owner]
	if !ok {
		return "", false
	}
	return strconv.FormatUint(uint64(port), 10), true
}

// Release returns an owner's port to the pool. Idempotent.
//
// Called when a session disappears from the assignment set, not when its worker restarts — the
// distinction is the whole point of owner-keyed allocation.
func (a *Allocator) Release(owner string) {
	a.mu.Lock()
	defer a.mu.Unlock()

	port, ok := a.byName[owner]
	if !ok {
		return
	}
	delete(a.byName, owner)
	delete(a.inUse, port)
}

// Owners returns every owner holding a port, sorted. Diagnostics.
func (a *Allocator) Owners() []string {
	a.mu.Lock()
	defer a.mu.Unlock()

	owners := make([]string, 0, len(a.byName))
	for owner := range a.byName {
		owners = append(owners, owner)
	}
	slices.Sort(owners)
	return owners
}

func (a *Allocator) advanceLocked() {
	if a.cursor >= a.rng.High {
		a.cursor = a.rng.Low
		return
	}
	a.cursor++
}

// freeLocked probe-binds a port to see whether anything else on the host holds it.
//
// The socket is opened and closed immediately; the worker binds it properly a moment later. That
// window is not a race worth closing, because the allocator's own bookkeeping already excludes
// every port *this* agent handed out, and node-name exclusivity means there is no second agent on
// the host to race with (§7.1). What this catches is the other thing entirely: a port in the
// configured range that some unrelated process on the node happens to be using.
func (a *Allocator) freeLocked(port uint16) bool {
	address := net.JoinHostPort("", strconv.FormatUint(uint64(port), 10))
	listener, err := a.listen("tcp", address)
	if err != nil {
		return false
	}
	_ = listener.Close()
	return true
}

// shouldProbe reports whether a TCP probe-bind says anything true about a provider's endpoint.
//
// **Plan decision — probe-bind only where the port lives in the kernel's TCP table.** M5d asks
// for probe-binding to detect collisions and says to skip it for shm, whose service is a name
// rather than a port. The same argument disqualifies verbs and efa for a different reason: their
// service is a port in the RDMA CM's own port space, which is a separate table from the kernel's
// TCP one, so a successful TCP bind would prove nothing about whether the fabric endpoint is free
// and a failed one would refuse a port that is perfectly usable — a false negative that silently
// shrinks the operator's configured range.
//
// So the probe runs for tcp, where it is ground truth, and elsewhere the allocator's own
// bookkeeping is the only claim it makes. That is not a downgrade: on a node running one agent,
// the agent already knows every fabric port it handed out, and a collision with a foreign process
// in a range the operator reserved for this is not a case any of these providers can detect
// before binding anyway.
func shouldProbe(provider api.Provider) bool { return provider == api.ProviderTCP }
