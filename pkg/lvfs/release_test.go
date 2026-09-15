package lvfs

import (
	"encoding/xml"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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

func TestReleaseTimestampAndSizesAreOmittedWhenUnknown(t *testing.T) {
	out, err := xml.Marshal(Release{Version: "1.0", Date: "2026-01-01"})
	require.NoError(t, err)
	assert.NotContains(t, string(out), "timestamp")
	assert.NotContains(t, string(out), "<size")
}

func TestReleaseTimestampAndSizes(t *testing.T) {
	out, err := xml.Marshal(Release{
		Version:   "1.0",
		Date:      "2026-01-01",
		Timestamp: 1767225600,
		Sizes:     []Size{{Type: "installed", Bytes: 123}, {Type: "download", Bytes: 456}},
	})
	require.NoError(t, err)
	assert.Contains(t, string(out), `timestamp="1767225600"`)
	assert.Contains(t, string(out), `<size type="installed">123</size><size type="download">456</size>`)
}

func TestNewestRelease(t *testing.T) {
	assert.Nil(t, NewestRelease(nil))

	releases := []Release{
		{Version: "10.0", Date: "2026-01-01"},
		{Version: "9.0", Date: "2026-02-01"},
		{Version: "20.0", Date: "2025-12-01"},
	}
	assert.Equal(t, "9.0", NewestRelease(releases).Version, "the release published last, whatever its version looks like")
}

func TestNewestReleaseOfOneDayDoesNotDependOnOrder(t *testing.T) {
	a := Release{Version: "1.0", Date: "2026-01-01", Checksums: []Checksum{{Target: "content", Type: "sha256", Value: "aaa"}}}
	b := Release{Version: "2.0", Date: "2026-01-01", Checksums: []Checksum{{Target: "content", Type: "sha256", Value: "bbb"}}}

	assert.Equal(t, NewestRelease([]Release{a, b}).Version, NewestRelease([]Release{b, a}).Version)
}

func TestNewestIndex(t *testing.T) {
	releases := []Release{
		{Version: "1.0", Date: "2026-01-01"},
		{Version: "2.0", Date: "2026-03-01"},
		{Version: "1.5", Date: "2026-02-01"},
	}
	byVersion := func(versions ...string) func(Release) bool {
		return func(release Release) bool { return slices.Contains(versions, release.Version) }
	}

	assert.Equal(t, 1, NewestIndex(releases, byVersion("1.0", "2.0", "1.5")))
	assert.Equal(t, 2, NewestIndex(releases, byVersion("1.0", "1.5")), "the newest of those that match")
	assert.Equal(t, -1, NewestIndex(releases, byVersion("9.9")))
}

func TestNewestReleaseOfOneDayGoesByTimestamp(t *testing.T) {
	morning := Release{Version: "1.0", Date: "2026-01-01", Timestamp: 1767254400, Checksums: []Checksum{{Target: "content", Type: "sha256", Value: "aaa"}}}
	evening := Release{Version: "1.1", Date: "2026-01-01", Timestamp: 1767283200, Checksums: []Checksum{{Target: "content", Type: "sha256", Value: "zzz"}}}

	assert.Equal(t, "1.1", NewestRelease([]Release{morning, evening}).Version, "the checksum would have picked the morning one")
	assert.Equal(t, "1.1", NewestRelease([]Release{evening, morning}).Version)
}

func TestNewestReleaseOfOneDayIsTheSameWhateverTheOrder(t *testing.T) {
	// Timestamps on some releases only must still give one answer, whatever the order.
	a := Release{Version: "a", Date: "2026-01-01", Timestamp: 20, Checksums: []Checksum{{Target: "content", Type: "sha256", Value: "z"}}}
	b := Release{Version: "b", Date: "2026-01-01", Timestamp: 10, Checksums: []Checksum{{Target: "content", Type: "sha256", Value: "a"}}}
	c := Release{Version: "c", Date: "2026-01-01", Checksums: []Checksum{{Target: "content", Type: "sha256", Value: "m"}}}

	for _, order := range [][]Release{{a, b, c}, {b, c, a}, {c, a, b}, {c, b, a}} {
		assert.Equal(t, "a", NewestRelease(order).Version)
	}
}
