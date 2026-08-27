package epoch

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// realTargetInfo is what mxl 1.1.0-rc1 actually wrote, captured from a tcp target on loopback.
// A synthetic fixture would prove nothing about the coupling this package's unknown-field
// guard exists to watch.
func realTargetInfo(t *testing.T) string {
	t.Helper()
	blob, err := os.ReadFile(filepath.Join("testdata", "target-info-tcp.json"))
	require.NoError(t, err)
	return string(blob)
}

func TestDecodeRealTargetInfo(t *testing.T) {
	t.Parallel()

	info, unknown, err := Decode(realTargetInfo(t))
	require.NoError(t, err)

	// Zero unknown fields is the assertion that matters: it is the live half of the
	// mxl-fabrics coupling guard. When it starts failing, the library grew a field and someone
	// has to decide whether it belongs in the epoch.
	assert.Empty(t, unknown)

	assert.Equal(t, "944718547066549234", info.ID)
	assert.Equal(t, "tcp", info.Provider)
	assert.Equal(t, 2, info.AddressFormat)
	assert.Equal(t, "AgBhp38AAAEAAAAAAAAAAA==", info.FabricAddress)

	require.Len(t, info.Regions, 5)
	// rkeys exceed math.MaxInt64, which is exactly why they are serialised as strings.
	assert.Equal(t, U64(17918262359965949928), info.Regions[0].RKey)
	assert.Equal(t, U64(5537792), info.Regions[0].Len)

	// addr is "0" on this provider — it does not report a mapping address at all. Worth
	// asserting, because §5.2 reasons about addr as an ASLR-randomised mmap address, and on tcp
	// it carries no entropy whatsoever. That is the empirical case for the incarnation nonce.
	assert.Equal(t, U64(0), info.Regions[0].Addr)

	// Discrete (video) flows have no bounce buffer; it is a continuous-flow structure.
	assert.Nil(t, info.BounceBuffer)
}

func TestDecodeReportsUnknownFieldPaths(t *testing.T) {
	t.Parallel()

	const blob = `{
		"id": "1",
		"fabricAddress": "AA==",
		"newTopLevel": 1,
		"regions": [{"addr":"0","len":"1","rkey":"2"},{"addr":"0","len":"1","rkey":"3","perRegionKnob":true}],
		"bounceBufferInfo": {"entryCount":"4","entrySize":"5","alignment":64}
	}`

	_, unknown, err := Decode(blob)
	require.NoError(t, err)
	assert.Equal(t, []string{"bounceBufferInfo.alignment", "newTopLevel", "regions[1].perRegionKnob"}, unknown)
}

func TestDecodeRejectsUnusableBlobs(t *testing.T) {
	t.Parallel()

	for name, blob := range map[string]string{
		"empty":            ``,
		"whitespace only":  "  \n",
		"not json":         `not json`,
		"not an object":    `[1,2,3]`,
		"truncated":        `{"id":"1","fabricAddress":"AA==","regions":[{"addr":"0",`,
		"missing id":       `{"fabricAddress":"AA==","regions":[]}`,
		"empty id":         `{"id":"","fabricAddress":"AA=="}`,
		"unparseable rkey": `{"id":"1","regions":[{"addr":"0","len":"1","rkey":"not-a-number"}]}`,
		"negative len":     `{"id":"1","regions":[{"addr":"0","len":"-1","rkey":"1"}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, _, err := Decode(blob)
			assert.Error(t, err)
		})
	}
}

// A pre-M0 worker wrote the blob with the library's NUL terminator included. Tolerating it
// keeps a mixed-version deployment from looking like a corrupt blob.
func TestDecodeToleratesTrailingNUL(t *testing.T) {
	t.Parallel()

	withNUL, unknown, err := Decode(realTargetInfo(t) + "\x00")
	require.NoError(t, err)
	assert.Empty(t, unknown)

	clean, _, err := Decode(realTargetInfo(t))
	require.NoError(t, err)

	// And it must not perturb the epoch: the two blobs are the same target info.
	assert.Equal(t, Compute("n", clean), Compute("n", withNUL))
}

func TestDecodeAudioBlobWithBounceBuffer(t *testing.T) {
	t.Parallel()

	const blob = `{"id":"7","addressFormat":2,"provider":"verbs","fabricAddress":"AA==",
		"regions":[{"addr":"140234","len":"4096","rkey":"9"}],
		"bounceBufferInfo":{"entryCount":"128","entrySize":"1024"}}`

	info, unknown, err := Decode(blob)
	require.NoError(t, err)
	assert.Empty(t, unknown)

	require.NotNil(t, info.BounceBuffer)
	assert.Equal(t, U64(128), info.BounceBuffer.EntryCount)
	assert.Equal(t, U64(1024), info.BounceBuffer.EntrySize)
}

// The library quotes these values today. Accepting a bare number too means a library that
// stops quoting them does not break the epoch.
func TestU64AcceptsStringOrNumber(t *testing.T) {
	t.Parallel()

	var quoted, bare Region
	require.NoError(t, json.Unmarshal([]byte(`{"addr":"1","len":"2","rkey":"18446744073709551615"}`), &quoted))
	require.NoError(t, json.Unmarshal([]byte(`{"addr":1,"len":2,"rkey":18446744073709551615}`), &bare))
	assert.Equal(t, quoted, bare)
	assert.Equal(t, U64(^uint64(0)), quoted.RKey)

	encoded, err := json.Marshal(quoted)
	require.NoError(t, err)
	assert.JSONEq(t, `{"addr":"1","len":"2","rkey":"18446744073709551615"}`, string(encoded))
}
