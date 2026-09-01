package reconcile

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jonasohland/mxl-replicator/internal/api"
	"github.com/jonasohland/mxl-replicator/internal/server/state"
)

// --- parked destinations (§9.1, §11) -----------------------------------------------------------
//
// `disabled` is the only place a request spells *off*, and the behaviour it buys is entirely
// subtractive: a parked destination produces no pairing, so no leg, no path, no session and no
// assignment. Everything below is that one sentence checked from a different side.

// park returns the destinations with the one at `index` switched off.
func park(destinations []api.Destination, index int) []api.Destination {
	out := make([]api.Destination, len(destinations))
	copy(out, destinations)
	out[index].Disabled = true
	return out
}

// dst is a destination on `edge-01`, so a test can name a second one without repeating the shape.
func dst(node string, elements ...string) api.Destination {
	return api.Destination{Node: node, Domain: api.Domain{Area: "fast", Elements: elements}}
}

// The base case: one request, one destination, parked. Nothing is asked of the fleet and the
// request says so in a word that means neither "broken" nor "coming".
func TestAParkedDestinationExpandsToNothing(t *testing.T) {
	t.Parallel()

	spec := flowRequest("cam1")
	spec.Destinations = park(spec.Destinations, 0)

	fleet := base().request("cam1", spec).build()
	result := Compute(fleet, Config{})

	assert.Empty(t, result.Paths, "a parked destination is not a pairing, so it expands to nothing")
	assert.Empty(t, result.Sessions)

	status := result.Requests[rid("cam1")]
	assert.Equal(t, api.StateDisabled, status.State)
	assert.Equal(t, api.ReasonAllDestinationsDisabled, status.ReasonCode)

	// The reason names what would come back, which is the question somebody reads this to answer
	// three months after parking it.
	assert.Contains(t, status.Reason, "edge-01/fast/ingest")

	// No assignment reaches either node: the flag is not a request the agent has to know about.
	for _, node := range []string{"studio-a", "edge-01"} {
		assert.Empty(t, result.Assignments[node].Assignments, "node %s", node)
	}
}

// The distinction that has to survive: a request expanding to nothing because everything is parked
// and one expanding to nothing because its selector matches nothing are different conditions, and
// only the second resolves by itself.
func TestParkedIsNotTheSameAsWaiting(t *testing.T) {
	t.Parallel()

	parked := flowRequest("parked")
	parked.Destinations = park(parked.Destinations, 0)

	unmatched := flowRequest("unmatched")
	unmatched.Sources[0].Select = api.Selector{Flow: "flow-nobody-publishes"}

	fleet := base().request("parked", parked).request("unmatched", unmatched).build()
	result := Compute(fleet, Config{})

	assert.Equal(t, api.StateDisabled, result.Requests[rid("parked")].State)
	assert.Equal(t, api.StateWaiting, result.Requests[rid("unmatched")].State)
	assert.Equal(t, api.ReasonFlowNotFound, result.Requests[rid("unmatched")].ReasonCode)
}

// A rectangle with one column switched off is not disabled — it folds over the paths it still has,
// exactly as though the parked entry had never been written.
func TestAPartlyParkedRequestFoldsOverWhatIsLeft(t *testing.T) {
	t.Parallel()

	spec := flowRequest("cam1")
	spec.Destinations = []api.Destination{dst("edge-01", "ingest"), dst("edge-01", "archive")}
	spec.Destinations = park(spec.Destinations, 1)

	fleet := base().request("cam1", spec).build()
	result := Compute(fleet, Config{})

	status := result.Requests[rid("cam1")]
	assert.Equal(t, api.StateEstablishing, status.State)
	assert.NotEqual(t, api.StateDisabled, status.State, "one live leg is not a disabled request")

	require.Len(t, result.Paths, 1)
	assert.Equal(t, "fast/ingest", onlyPath(t, result).Destination.DomainName(),
		"the parked column produced no path")

	// The source row sees the same thing the request does: it has a live leg, so it is not dark.
	require.Len(t, status.Sources, 1)
	assert.Equal(t, api.StateEstablishing, status.Sources[0].State)
	assert.Len(t, status.Sources[0].Paths, 1)
}

// A source row of a *fully* parked request is dark for the request's reason and not for one of its
// own. Getting this wrong would send an operator to a studio that is fine.
func TestASourceRowOfAParkedRequestReadsDisabled(t *testing.T) {
	t.Parallel()

	spec := flowRequest("wall")
	spec.Sources = append(spec.Sources, wallSource("studio-b"))
	spec.Destinations = park(spec.Destinations, 0)

	fleet := base().
		node("studio-b").
		flow("studio-b", "media/cameras", api.FlowInventory{ID: "flow-b", Definition: flowDef, Producing: true}).
		request("wall", spec).
		build()

	status := Compute(fleet, Config{}).Requests[rid("wall")]
	require.Len(t, status.Sources, 2)
	for i, row := range status.Sources {
		assert.Equal(t, api.StateDisabled, row.State, "sources[%d]", i)
		assert.Equal(t, api.ReasonAllDestinationsDisabled, row.ReasonCode, "sources[%d]", i)
	}
}

// A parked destination is validated against nothing (§7.2): it names an area `edge-02` does not
// advertise, which would be `unknown_area` on a live leg, and the request establishes anyway.
func TestAParkedDestinationIsNotValidatedAgainstTheFleet(t *testing.T) {
	t.Parallel()

	spec := flowRequest("cam1")
	spec.Destinations = []api.Destination{
		dst("edge-01", "ingest"),
		{Node: "edge-02", Domain: api.Domain{Area: "nope", Elements: []string{"ingest"}}, Disabled: true},
	}

	fleet := base().node("edge-02").request("cam1", spec).build()
	result := Compute(fleet, Config{})

	status := result.Requests[rid("cam1")]
	assert.Equal(t, api.StateEstablishing, status.State)
	assert.Empty(t, status.ReasonCode, "a parked leg has no verdict to report")
	assert.Len(t, result.Paths, 1)

	// And it is refused the moment it is switched on, so nothing has been hidden — only deferred.
	spec.Destinations[1].Disabled = false
	enabled := base().node("edge-02").request("cam1", spec).build()
	assert.Equal(t, api.ReasonUnknownArea, Compute(enabled, Config{}).Requests[rid("cam1")].ReasonCode)
}

// Parking releases the path, so another request in an exclusive namespace may take it — and the
// re-enabled one then loses, because incumbency leads the precedence order (§9.3, §7.5).
func TestParkingReleasesThePathToAnotherRequestInAnExclusiveNamespace(t *testing.T) {
	t.Parallel()

	parked := flowRequest("cam1")
	parked.Destinations = park(parked.Destinations, 0)

	fleet := base().namespace(api.DefaultNamespace, api.PathsExclusive).request("cam1", parked).build()
	requestAt(&fleetBuilder{fleet: fleet}, "cam1-again", flowRequest("cam1-again"), created.Add(time.Hour))

	result := Compute(fleet, Config{})

	// The newcomer holds the path outright: a parked request is not an incumbent, so no overlap
	// fires and nothing is INVALID.
	require.Len(t, result.Paths, 1)
	assert.Equal(t, []string{"default/cam1-again"}, onlyPath(t, result).Requests)
	assert.Equal(t, api.StateDisabled, result.Requests[rid("cam1")].State)
	assert.Equal(t, api.StateEstablishing, result.Requests[rid("cam1-again")].State)
	assert.Empty(t, result.Requests[rid("cam1-again")].ReasonCode)

	// Switching the first one back on is not the inverse of switching it off, and the mechanism is
	// worth pinning because it is *not* the one §7.5 leads with. `namespaceOverlaps` orders legs by
	// `(UpdatedAt, ID)` and consults no session record, so the contest between two requests over one
	// path is decided by recency alone — incumbency cannot distinguish them, since both expand onto
	// the same path and that path's session exists whoever holds it.
	//
	// What makes the loss reliable is that un-parking is a *write*: the server stamps `UpdatedAt` on
	// every request write, so a request coming back always carries the newest one and always sorts
	// last among the contenders.
	revived := base().namespace(api.DefaultNamespace, api.PathsExclusive).build()
	for id, session := range result.Sessions {
		revived.Sessions[id] = state.Entry[state.SessionRecord]{Found: true, Value: session}
	}
	requestAt(&fleetBuilder{fleet: revived}, "cam1-again", flowRequest("cam1-again"), created.Add(time.Hour))
	requestAt(&fleetBuilder{fleet: revived}, "cam1", flowRequest("cam1"), created.Add(2*time.Hour))

	after := Compute(revived, Config{})
	assert.Equal(t, []string{"default/cam1-again"}, onlyPath(t, after).Requests)
	assert.Equal(t, api.ReasonNamespaceOverlap, after.Requests[rid("cam1")].ReasonCode)

	// The other half of the same fact, and the reason the comment above says "recency" rather than
	// "incumbency": a running session buys the request holding it nothing. Rewind the returning
	// request's stamp and it takes the path straight back off the one that has been carrying it.
	rewound := base().namespace(api.DefaultNamespace, api.PathsExclusive).build()
	for id, session := range result.Sessions {
		rewound.Sessions[id] = state.Entry[state.SessionRecord]{Found: true, Value: session}
	}
	requestAt(&fleetBuilder{fleet: rewound}, "cam1-again", flowRequest("cam1-again"), created.Add(time.Hour))
	requestAt(&fleetBuilder{fleet: rewound}, "cam1", flowRequest("cam1"), created)

	back := Compute(rewound, Config{})
	assert.Equal(t, []string{"default/cam1"}, onlyPath(t, back).Requests)
	assert.Equal(t, api.ReasonNamespaceOverlap, back.Requests[rid("cam1-again")].ReasonCode)
}

// DISABLED is aggregate-only, like PARTIAL and for a sharper reason: a parked destination produces
// no path, so there is nothing underneath for the word to be about.
func TestNoPathIsEverDisabled(t *testing.T) {
	t.Parallel()

	spec := flowRequest("cam1")
	spec.Destinations = []api.Destination{dst("edge-01", "ingest"), dst("edge-01", "archive")}
	spec.Destinations = park(spec.Destinations, 1)

	result := Compute(base().request("cam1", spec).build(), Config{})
	for _, path := range result.Paths {
		assert.NotEqual(t, api.StateDisabled, path.State)
	}

	assert.NotContains(t, api.States(), api.StateDisabled, "not a state one thing can be in")
	assert.Contains(t, api.RequestStates(), api.StateDisabled)
}
