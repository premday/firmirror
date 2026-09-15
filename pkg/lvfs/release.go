package lvfs

import (
	"cmp"
	"slices"
	"strings"
)

// SortReleases orders releases newest first by date, then timestamp, then
// checksum, so the published document is byte-stable across runs. ProcessVendor
// accumulates components in whatever order its workers finish, so without this
// the document differs run to run even when nothing changed. Checksums provide a
// stable tiebreaker without interpreting vendor-specific version formats.
func SortReleases(releases []Release) {
	slices.SortStableFunc(releases, compareReleases)
}

// compareReleases orders newest first by date, then timestamp (a release without
// one is older), then checksum and location, which only make the order stable.
func compareReleases(a, b Release) int {
	if c := strings.Compare(b.Date, a.Date); c != 0 {
		return c
	}
	if c := cmp.Compare(b.Timestamp, a.Timestamp); c != 0 {
		return c
	}
	if c := strings.Compare(releaseChecksum(a), releaseChecksum(b)); c != 0 {
		return c
	}
	return strings.Compare(a.Location, b.Location)
}

// Newer reports whether a sorts before b in SortReleases order, so every caller
// breaks ties between releases of the same day the same way.
func Newer(a, b Release) bool {
	return compareReleases(a, b) < 0
}

// NewestRelease returns the newest release, or nil when there is none. It goes by
// date and not by version, as vendor versions are not semver ("2.90",
// "7.30.30.51", "HPG4"); ties on the date fall back to the Newer order.
func NewestRelease(releases []Release) *Release {
	if index := NewestIndex(releases, func(Release) bool { return true }); index >= 0 {
		return &releases[index]
	}
	return nil
}

// NewestIndex returns the index of the newest release matching match, or -1.
func NewestIndex(releases []Release, match func(Release) bool) int {
	index := -1
	for i := range releases {
		if match(releases[i]) && (index < 0 || Newer(releases[i], releases[index])) {
			index = i
		}
	}
	return index
}

// releaseChecksum returns the SHA-256 content checksum when available, falling
// back to another content checksum for older or incomplete metadata.
func releaseChecksum(release Release) string {
	for _, checksum := range release.Checksums {
		if checksum.Target == "content" && checksum.Type == "sha256" {
			return checksum.Value
		}
	}
	for _, checksum := range release.Checksums {
		if checksum.Target == "content" {
			return checksum.Value
		}
	}
	return ""
}
