// Package client speaks the control-plane HTTP APIs from the outside.
//
// It exists so that the agent, `xpt` and the importer are all written against one description
// of the wire rather than three, and so that the *combined* deployment has no shortcut: when
// `mxl-replicator run` starts both roles in one process the co-located agent still dials its
// server over loopback through this package, because there being exactly one code path is what
// stops the combined form developing behaviour the distributed form lacks (§2.2).
//
// # What this package owes the agent
//
// One property, and it is the one with the worst failure mode in the system: **a failed call
// and an empty answer must never be spelled the same way** (§4.2). Every method here either
// returns a value the server actually sent or returns an error — never a usable zero value
// alongside a nil error, and never a decoded body built from a response that was not a 200. In
// particular [Client.Assignments] returns a nil set with an error for [api.CodeNotReady], so
// that a caller which forgets to check the error cannot reconcile against "no assignments" and
// tear down every worker on the node.
//
// Only the agent API is implemented here. The user-API client belongs with the tools that need
// it (M8's importer, M9's `xpt`) and is not on any path the agent takes.
package client
