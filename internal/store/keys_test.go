package store

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestKeysLandInTheRightLayer is the check that matters for §4: an agent report must never be
// able to reach desired state, and the key space is where that is visible.
func TestKeysLandInTheRightLayer(t *testing.T) {
	t.Parallel()

	desired := []string{NodeKey("edge-01"), NamespaceKey("nab"), RequestKey("nab", "r-1"), KeyPolicy}
	observed := []string{LeaseKey("edge-01"), InventoryKey("edge-01"), StatusKey("edge-01")}
	derived := []string{SessionKey("s-1"), AssignmentsKey("edge-01")}

	for _, key := range desired {
		assert.True(t, strings.HasPrefix(key, PrefixDesired), "%s is not desired state", key)
	}
	for _, key := range observed {
		assert.True(t, strings.HasPrefix(key, PrefixObserved), "%s is not observed state", key)
	}
	for _, key := range derived {
		assert.True(t, strings.HasPrefix(key, PrefixDerived), "%s is not derived state", key)
	}
}

// TestSnapshotPrefixCoversEveryLayerAndNothingElse is the test [PrefixSnapshot] names.
//
// The fleet snapshot is one List over that prefix (§7.3), so a layer outside it would be silently
// absent from every snapshot — which is indistinguishable from a wiped store, and is the failure
// §4.2 exists to prevent. A fourth layer prefix belongs in this test before it belongs in a key.
//
// The other half matters just as much and in the other direction: the event log and leader
// election must stay **out**, or a diagnostic write lands in the reconciler's snapshot and wakes
// its watch (§12.1).
func TestSnapshotPrefixCoversEveryLayerAndNothingElse(t *testing.T) {
	t.Parallel()

	for _, prefix := range []string{PrefixDesired, PrefixObserved, PrefixDerived} {
		assert.True(t, strings.HasPrefix(prefix, PrefixSnapshot),
			"layer %s is outside the snapshot prefix %s — it would be invisible to every reconcile",
			prefix, PrefixSnapshot)
	}
	for _, key := range []string{KeyPolicy, KeyReconciler} {
		assert.True(t, strings.HasPrefix(key, PrefixSnapshot), "%s is outside the snapshot", key)
	}

	for _, prefix := range []string{PrefixEvents, PrefixElection} {
		assert.False(t, strings.HasPrefix(prefix, PrefixSnapshot),
			"%s is inside the snapshot prefix %s", prefix, PrefixSnapshot)
	}
	for _, key := range []string{
		PathEventsKey("p-1"), RequestEventsKey("nab", "r-1"), NodeEventsKey("edge-01"), LogKey("p-1"),
	} {
		assert.False(t, strings.HasPrefix(key, PrefixSnapshot), "%s is inside the snapshot", key)
		assert.True(t, strings.HasPrefix(key, PrefixEvents), "%s is not an event key", key)
	}
}

func TestKeysAreUnderTheirListPrefix(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct{ key, prefix string }{
		{NodeKey("edge-01"), PrefixNodes},
		{NamespaceKey("nab"), PrefixNamespaces},
		{RequestKey("nab", "r-1"), PrefixRequests},
		{LeaseKey("edge-01"), PrefixLeases},
		{InventoryKey("edge-01"), PrefixInventory},
		{StatusKey("edge-01"), PrefixStatus},
		{SessionKey("s-1"), PrefixSessions},
		{AssignmentsKey("edge-01"), PrefixAssignments},
	} {
		assert.True(t, strings.HasPrefix(tc.key, tc.prefix), "%s is not under %s", tc.key, tc.prefix)
	}
}

// TestNodeNamesCannotEscapeTheirPrefix is the reason escape() exists. A node name is
// operator-assigned free-form text, validated for fleet-wide uniqueness (§7.1) and nothing
// else, so a name containing a slash would otherwise write outside the prefix it belongs to —
// at best invisible to every list, at worst landing on another node's key.
func TestNodeNamesCannotEscapeTheirPrefix(t *testing.T) {
	t.Parallel()

	for _, name := range []string{
		"edge-01/../../desired/policy",
		"a/b",
		"../nodes/other",
		"with space",
	} {
		key := InventoryKey(name)
		assert.True(t, strings.HasPrefix(key, PrefixInventory), "%q escaped its prefix: %s", name, key)
		assert.Equal(t, strings.Count(PrefixInventory, "/"), strings.Count(key, "/"),
			"%q introduced a path separator: %s", name, key)
	}
}

func TestDistinctNodesGetDistinctKeys(t *testing.T) {
	t.Parallel()

	assert.NotEqual(t, InventoryKey("a/b"), InventoryKey("a%2Fb"),
		"escaping must not collapse two different node names onto one key")
}

// TestSiblingPrefixesDoNotOverlap pins the property the trailing slashes exist for: prefixes
// match raw bytes, so "/desired/node" would otherwise select "/desired/nodes/edge-01".
func TestSiblingPrefixesDoNotOverlap(t *testing.T) {
	t.Parallel()

	all := []string{
		PrefixNodes, PrefixRequests,
		PrefixLeases, PrefixInventory, PrefixStatus,
		PrefixSessions, PrefixAssignments,
	}
	for i, a := range all {
		assert.True(t, strings.HasSuffix(a, "/"), "%s must end in a slash", a)
		for j, b := range all {
			if i != j {
				assert.False(t, strings.HasPrefix(a, b), "%s is inside %s", a, b)
			}
		}
	}
}

// **A domain name contains `/`**, and the key layout depends on [escape] percent-encoding it so
// that the record stays one key segment (§8).
//
// The property belongs to url.PathEscape's escaping mode rather than to anything in this tree,
// which is why it is pinned here rather than left to a comment: if the standard library ever
// stopped encoding `/` in that mode, `/desired/domains/edge-01/fast/ingest` would be three
// segments and the per-node prefix scan would split in the wrong place.
func TestADomainNameWithASeparatorStaysOneKeySegment(t *testing.T) {
	t.Parallel()

	key := DomainLabelsKey("edge-01", "fast/ingest")
	prefix := DomainLabelsPrefix("edge-01")

	assert.True(t, strings.HasPrefix(key, prefix), "%s is not under %s", key, prefix)
	assert.Equal(t, PrefixDesired+"domains/edge-01/fast%2Fingest", key)

	// One segment past the prefix, which is what makes the two-level scan split where it is meant
	// to. Three would put `ingest` under a node called `fast`.
	assert.NotContains(t, strings.TrimPrefix(key, prefix), "/")

	// And a node name with a separator in it is escaped for the same reason (§7.1).
	nested := DomainLabelsKey("rack/edge-01", "fast/ingest")
	assert.Equal(t, PrefixDesired+"domains/rack%2Fedge-01/fast%2Fingest", nested)
	assert.NotEqual(t, DomainLabelsPrefix("rack"), DomainLabelsPrefix("rack/edge-01"))
}
