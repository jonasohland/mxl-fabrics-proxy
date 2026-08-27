package epoch

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func decode(t *testing.T, blob string) *TargetInfo {
	t.Helper()
	info, _, err := Decode(blob)
	require.NoError(t, err)
	return info
}

// The entire reason the nonce exists (§5.2).
//
// A target that restarts and reports a byte-identical blob must still produce a different
// epoch, or the initiator keeps running against rkeys that no longer exist: no error, no data,
// everything upstream reporting healthy. That is the worst failure mode in the system, and it
// is not hypothetical — on tcp the only field that varies across restarts is the rkey, and
// nothing contractually promises it varies.
func TestIdenticalTargetInfoWithNewNonceGivesNewEpoch(t *testing.T) {
	t.Parallel()

	info := decode(t, realTargetInfo(t))
	same := decode(t, realTargetInfo(t))

	first := Compute(NewNonce(), info)
	second := Compute(NewNonce(), same)

	assert.NotEqual(t, first, second)

	// Same nonce, same blob: stable. The epoch is a function of its inputs and nothing else —
	// no clock, no counter, no state on disk.
	nonce := NewNonce()
	assert.Equal(t, Compute(nonce, info), Compute(nonce, same))
}

// An unknown field warns and does not change the epoch (§5.2). Failing closed would take out
// replication on an unrelated mxl upgrade; silently rehashing would make the epoch change for a
// reason that has nothing to do with the memory registration.
func TestUnknownFieldWarnsWithoutChangingTheEpoch(t *testing.T) {
	t.Parallel()

	const nonce = "NONCE"

	base := realTargetInfo(t)
	extended := strings.Replace(base, `{"addressFormat":2,`, `{"addressFormat":2,"futureField":{"nested":[1,2,3]},`, 1)
	require.NotEqual(t, base, extended)

	original := decode(t, base)

	info, unknown, err := Decode(extended)
	require.NoError(t, err)
	assert.Equal(t, []string{"futureField"}, unknown)

	assert.Equal(t, Compute(nonce, original), Compute(nonce, info))
}

// Field selection is a decision, not an accident: these are the fields whose change means the
// initiator must reconnect (§5.2).
func TestEpochTracksTheHashedFields(t *testing.T) {
	t.Parallel()

	const nonce = "NONCE"
	const base = `{"id":"1","addressFormat":2,"provider":"verbs","fabricAddress":"AgBhpw==","regions":[{"addr":"4096","len":"8192","rkey":"77"}],"bounceBufferInfo":{"entryCount":"128","entrySize":"1024"}}`
	const noBounce = `{"id":"1","addressFormat":2,"provider":"verbs","fabricAddress":"AgBhpw==","regions":[{"addr":"4096","len":"8192","rkey":"77"}]}`

	unchanged := Compute(nonce, decode(t, base))

	t.Run("changes the epoch", func(t *testing.T) {
		t.Parallel()
		for name, blob := range map[string]string{
			"fabricAddress":           strings.Replace(base, `"AgBhpw=="`, `"AgBhqA=="`, 1),
			"region addr":             strings.Replace(base, `"addr":"4096"`, `"addr":"8192"`, 1),
			"region len":              strings.Replace(base, `"len":"8192"`, `"len":"4096"`, 1),
			"region rkey":             strings.Replace(base, `"rkey":"77"`, `"rkey":"78"`, 1),
			"an added region":         strings.Replace(base, `"rkey":"77"}]`, `"rkey":"77"},{"addr":"0","len":"0","rkey":"0"}]`, 1),
			"bounce entryCount":       strings.Replace(base, `"entryCount":"128"`, `"entryCount":"256"`, 1),
			"bounce entrySize":        strings.Replace(base, `"entrySize":"1024"`, `"entrySize":"512"`, 1),
			"a removed bounce buffer": noBounce,
		} {
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				assert.NotEqual(t, unchanged, Compute(nonce, decode(t, blob)))
			})
		}
	})

	t.Run("does not change the epoch", func(t *testing.T) {
		t.Parallel()
		// provider and addressFormat only ever change with a new session, because the server
		// assigns the provider (§5.2). id is an endpoint identifier of unspecified derivation
		// and says nothing about the memory registration.
		for name, blob := range map[string]string{
			"id":            strings.Replace(base, `"id":"1"`, `"id":"999"`, 1),
			"provider":      strings.Replace(base, `"provider":"verbs"`, `"provider":"tcp"`, 1),
			"addressFormat": strings.Replace(base, `"addressFormat":2`, `"addressFormat":3`, 1),
		} {
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				assert.Equal(t, unchanged, Compute(nonce, decode(t, blob)))
			})
		}
	})
}

// The source agent recomputes before starting an initiator (§5.3 step 6), which is what catches
// a mismatched or truncated blob before it reaches a worker that would silently move no data.
func TestVerify(t *testing.T) {
	t.Parallel()

	info := decode(t, realTargetInfo(t))
	assigned := Compute(NewNonce(), info)

	require.NoError(t, Verify(assigned, info))

	// The blob the initiator was handed is not the one the epoch was computed from.
	other := decode(t, strings.Replace(realTargetInfo(t), `"rkey":"17918262359965949928"`, `"rkey":"1"`, 1))
	assert.Error(t, Verify(assigned, other))

	// What Verify deliberately does *not* catch: an epoch and a blob that agree with each other
	// but are both stale. Verify checks one pair for internal consistency; noticing that the
	// pair is old is the reconcile loop's equality test against the assigned epoch, and the two
	// checks answer different questions.
	stale := Compute(NewNonce(), info)
	require.NoError(t, Verify(stale, info))
	assert.NotEqual(t, assigned, stale)
}

func TestVerifyRejectsMalformedEpochs(t *testing.T) {
	t.Parallel()

	info := decode(t, realTargetInfo(t))
	for _, assigned := range []string{"", "no-separator", ":onlydigest"} {
		assert.Error(t, Verify(assigned, info))
	}
}

// The framing is part of the wire contract: a target and an initiator on different agents must
// derive the same string from the same inputs, and during a rolling upgrade they may not be
// running the same build. Pin the output so a refactor that reorders or reframes a field is a
// test failure here rather than a silent refusal to connect in the field.
func TestEpochGolden(t *testing.T) {
	t.Parallel()

	const nonce = "TESTNONCE"
	const want = "TESTNONCE:7b49cb100e30c5f44b05ab5a6fe56c767b0c088b8683edbd712ef5a6a0e1903b"

	assert.Equal(t, want, Compute(nonce, decode(t, realTargetInfo(t))))
}

func TestEpochShape(t *testing.T) {
	t.Parallel()

	nonce := NewNonce()
	assert.NotEmpty(t, nonce)
	assert.NotEqual(t, nonce, NewNonce())
	assert.NotContains(t, nonce, nonceSeparator)

	epoch := Compute(nonce, decode(t, realTargetInfo(t)))
	prefix, digest, found := strings.Cut(epoch, nonceSeparator)
	require.True(t, found)
	assert.Equal(t, nonce, prefix)
	assert.Len(t, digest, 64)
}
