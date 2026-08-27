package ports

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseRange(t *testing.T) {
	r, err := ParseRange("24000-24999")
	require.NoError(t, err)
	assert.Equal(t, Range{Low: 24000, High: 24999}, r)
	assert.Equal(t, 1000, r.Count())
	assert.True(t, r.Contains(24000))
	assert.True(t, r.Contains(24999))
	assert.False(t, r.Contains(23999))
	assert.False(t, r.Contains(25000))

	single, err := ParseRange("24000")
	require.NoError(t, err)
	assert.Equal(t, Range{Low: 24000, High: 24000}, single)
	assert.Equal(t, 1, single.Count())
	assert.Equal(t, "24000", single.String())
}

func TestParseRangeRejects(t *testing.T) {
	for _, in := range []string{
		"",
		"-",
		"abc",
		"24999-24000", // inverted
		"0-100",       // port 0 is not bindable by the worker
		"1-70000",     // out of uint16
		"24000-",
	} {
		_, err := ParseRange(in)
		assert.Error(t, err, "input %q", in)
	}
}

func TestRangeRoundTrip(t *testing.T) {
	var r Range
	require.NoError(t, r.UnmarshalText([]byte("24000-24999")))

	text, err := r.MarshalText()
	require.NoError(t, err)
	assert.Equal(t, "24000-24999", string(text))

	var back Range
	require.NoError(t, back.UnmarshalText(text))
	assert.Equal(t, r, back)
}

func TestZeroRangeIsDetectable(t *testing.T) {
	// The trap: Range{}.Count() is 1, not 0, because {0,0} spans port 0. An unset range
	// must therefore be detected with IsZero, or a missing --port-range would look like a
	// usable one-port range and allocate port 0.
	assert.Equal(t, 1, Range{}.Count())
	assert.True(t, Range{}.IsZero())

	parsed, err := ParseRange("24000-24999")
	require.NoError(t, err)
	assert.False(t, parsed.IsZero())
}
