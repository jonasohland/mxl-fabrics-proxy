package version

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestGetNeverReportsAnEmptyVersion(t *testing.T) {
	// The version is reported at agent registration and the server refuses an agent newer
	// than itself (§13.1), so an empty string here becomes a confusing comparison there.
	assert.NotEmpty(t, Get().Version)
	assert.NotEmpty(t, String())
}

func TestLinkTimeVersionWins(t *testing.T) {
	original := Version
	t.Cleanup(func() { Version = original })

	Version = "0.0.4-4-g02eef87"
	assert.Equal(t, "0.0.4-4-g02eef87", Get().Version)
	assert.Contains(t, String(), "0.0.4-4-g02eef87")
}
