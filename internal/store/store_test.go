package store

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPutConfigResolvesOptions(t *testing.T) {
	t.Parallel()

	cfg, err := NewPutConfig(nil)
	require.NoError(t, err)
	assert.Zero(t, cfg.Lease)
	assert.False(t, cfg.HasIfRevision)
	assert.False(t, cfg.IfAbsent)

	cfg, err = NewPutConfig([]PutOpt{WithLease(7), IfRevision(42)})
	require.NoError(t, err)
	assert.Equal(t, LeaseID(7), cfg.Lease)
	assert.True(t, cfg.HasIfRevision)
	assert.Equal(t, int64(42), cfg.IfRevision)

	cfg, err = NewPutConfig([]PutOpt{IfAbsent()})
	require.NoError(t, err)
	assert.True(t, cfg.IfAbsent)

	// IfRevision(0) is a legitimate compare — an absent key compares as revision 0 — so the
	// zero value has to be distinguishable from the option not being set at all.
	cfg, err = NewPutConfig([]PutOpt{IfRevision(0)})
	require.NoError(t, err)
	assert.True(t, cfg.HasIfRevision)
	assert.Zero(t, cfg.IfRevision)
}

func TestPutConfigRejectsContradictoryCompares(t *testing.T) {
	t.Parallel()

	_, err := NewPutConfig([]PutOpt{IfAbsent(), IfRevision(3)})
	assert.Error(t, err, "IfAbsent and IfRevision assert different things about the same key")
}

func TestIfRevisionWorksForBothOperations(t *testing.T) {
	t.Parallel()

	opt := IfRevision(9)

	put, err := NewPutConfig([]PutOpt{opt})
	require.NoError(t, err)
	assert.Equal(t, int64(9), put.IfRevision)

	del, err := NewDelConfig([]DelOpt{opt})
	require.NoError(t, err)
	assert.True(t, del.HasIfRevision)
	assert.Equal(t, int64(9), del.IfRevision)
}

func TestEventTypeStrings(t *testing.T) {
	t.Parallel()

	assert.Equal(t, "put", EventPut.String())
	assert.Equal(t, "delete", EventDelete.String())
}
