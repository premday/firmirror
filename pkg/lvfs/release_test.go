package lvfs

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestSortReleases(t *testing.T) {
	releases := []Release{
		{
			Version: "99.0",
			Date:    "2025-01-01",
			Checksums: []Checksum{
				{Target: "content", Type: "sha1", Value: "zzz"},
				{Target: "content", Type: "sha256", Value: "aaa"},
			},
		},
		{
			Version: "2.0",
			Date:    "2026-01-01",
			Checksums: []Checksum{
				{Target: "content", Type: "sha256", Value: "ccc"},
			},
		},
		{
			Version: "1.0",
			Date:    "2025-01-01",
			Checksums: []Checksum{
				{Target: "content", Type: "sha256", Value: "bbb"},
			},
		},
	}

	SortReleases(releases)

	assert.Equal(t, []string{"2.0", "99.0", "1.0"}, []string{
		releases[0].Version,
		releases[1].Version,
		releases[2].Version,
	})
}

func TestReleaseChecksumFallsBackToAnotherContentChecksum(t *testing.T) {
	release := Release{Checksums: []Checksum{
		{Type: "sha256", Value: "artifact-without-a-target"},
		{Target: "content", Type: "sha1", Value: "content"},
	}}

	assert.Equal(t, "content", releaseChecksum(release))
}
