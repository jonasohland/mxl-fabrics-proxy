//go:build !linux

package agent

// SchedPrioAvailable is false anywhere the kernel interfaces the worker uses do not exist.
//
// The agent advertises only what it has verified (§10.2), and on a platform where this cannot be
// checked the honest answer is no: a request that asks for sched_prio then fails validation with
// a reason, rather than producing a worker that dies after its connection is established.
func SchedPrioAvailable() bool { return false }
