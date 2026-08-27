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

	desired := []string{NodeKey("edge-01"), RequestKey("r-1"), KeyPolicy}
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

func TestKeysAreUnderTheirListPrefix(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct{ key, prefix string }{
		{NodeKey("edge-01"), PrefixNodes},
		{RequestKey("r-1"), PrefixRequests},
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
