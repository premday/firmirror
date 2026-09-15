package lvfs

import (
	"slices"
	"strings"
)

// SortReleases orders releases newest first by date, then by checksum, so the
// published document is byte-stable across runs. ProcessVendor accumulates
// components in whatever order its workers finish, so without this the document
// differs run to run even when nothing changed. Checksums provide a stable
// tiebreaker without interpreting vendor-specific version formats.
func SortReleases(releases []Release) {
	slices.SortStableFunc(releases, func(a, b Release) int {
		if cmp := strings.Compare(b.Date, a.Date); cmp != 0 {
			return cmp
		}
		if cmp := strings.Compare(releaseChecksum(a), releaseChecksum(b)); cmp != 0 {
			return cmp
		}
		return strings.Compare(a.Location, b.Location)
	})
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
