// Package bkc decides which firmware version each rollout ring serves.
//
// A ring is a fwupd BKC tag: a host names it in HostBkc, and `fwupdmgr sync`
// moves each device to the release carrying it, downgrading if needed. What each
// ring serves is a YAML file edited by `firmirror bkc promote`; the refresh
// fetches it and tags the published releases accordingly.
package bkc

import (
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"maps"
	"regexp"
	"slices"
	"strings"

	"github.com/premday/firmirror/pkg/lvfs"
)

// DefaultTagPrefix is the tag prefix when the file names none.
const DefaultTagPrefix = "firmirror"

// DefaultRings are the rings of a new file, in promotion order.
var DefaultRings = []string{"dev", "beta", "preview", "stable"}

// namePattern constrains ring and prefix names: LVFS asks for lowercase tags
// without whitespace, and HostBkc is comma separated.
var namePattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`)

func (f *File) prefix() string {
	return cmp.Or(f.TagPrefix, DefaultTagPrefix)
}

// Tag returns the tag published for a ring.
func (f *File) Tag(ring string) string {
	return f.prefix() + "-" + ring
}

// CheckRing rejects a ring the file does not have.
func (f *File) CheckRing(ring string) error {
	if !slices.Contains(f.Rings, ring) {
		return fmt.Errorf("unknown ring %q, the rings are %s", ring, strings.Join(f.Rings, ", "))
	}
	return nil
}

// Upstream returns the ring a ring is promoted from, empty for the first ring,
// which takes the newest mirrored release.
func (f *File) Upstream(ring string) (string, error) {
	if err := f.CheckRing(ring); err != nil {
		return "", err
	}
	index := slices.Index(f.Rings, ring)
	if index == 0 {
		return "", nil
	}
	return f.Rings[index-1], nil
}

func (f *File) entries() map[string]*Entry {
	entries := make(map[string]*Entry, len(f.Components))
	for i := range f.Components {
		entries[f.Components[i].Key()] = &f.Components[i]
	}
	return entries
}

// Platform tells the platforms of one component ID apart: a short digest of the
// sorted, distinct GUIDs the component provides, empty when it provides none.
// GUIDs are what fwupd matches a device with, so the digest is as exact as it
// needs to be.
func Platform(guids []string) string {
	return digest(lvfs.DistinctGUIDs(guids))
}

func digest(guids []string) string {
	if len(guids) == 0 {
		return ""
	}
	sum := sha256.Sum256([]byte(strings.Join(guids, ",")))
	return hex.EncodeToString(sum[:8])
}

// platformOf uses the GUIDs lvfs.ComponentKey does, so the two agree on which
// components are one.
func platformOf(component lvfs.Component) string {
	return digest(lvfs.ComponentGUIDs(component))
}

// entryKey identifies a component by ID and platform.
func entryKey(id, platform string) string {
	return id + "\n" + platform
}

func keyOf(component lvfs.Component) string {
	return entryKey(component.ID, platformOf(component))
}

// OneLine collapses whitespace: some vendor catalogs put whole spec sheets,
// newlines included, in a name.
func OneLine(name string) string {
	return strings.Join(strings.Fields(name), " ")
}

// releaseIndex finds the newest release of a version. A vendor rebuild can
// publish the same version twice; only the newest is tagged so that fwupd never
// has two releases of one ring to choose from.
func releaseIndex(releases []lvfs.Release, version string) int {
	return lvfs.NewestIndex(releases, func(release lvfs.Release) bool { return release.Version == version })
}

// Gap is a version a ring serves that the mirror does not hold.
type Gap struct {
	Ring        string
	ComponentID string
	Name        string
	Version     string
}

// Overlap is a GUID shared by two components through which a ring serves two
// releases: another version, or the same version of another package.
type Overlap struct {
	Ring, GUID            string
	ComponentID, Version  string
	OtherID, OtherVersion string
}

// servedRelease is what a ring serves of a component; the location tells two
// packages of one version apart.
type servedRelease struct{ id, version, location string }

// overlaps records the release served through each of the GUIDs, in served, and
// returns those where another component already has the ring serve another
// release.
func overlaps(served map[string]servedRelease, component lvfs.Component, guids []string, ring string, release lvfs.Release) []Overlap {
	var found []Overlap
	for _, guid := range guids {
		slot := guid + "\n" + ring
		other, ok := served[slot]
		switch {
		case !ok:
			served[slot] = servedRelease{id: component.ID, version: release.Version, location: release.Location}
		case other.version != release.Version || other.location != release.Location:
			found = append(found, Overlap{Ring: ring, GUID: guid, ComponentID: component.ID, Version: release.Version, OtherID: other.id, OtherVersion: other.version})
		}
	}
	return found
}

// countVersion counts the releases of a version.
func countVersion(releases []lvfs.Release, version string) int {
	count := 0
	for _, release := range releases {
		if release.Version == version {
			count++
		}
	}
	return count
}

// Report is what Apply noticed. None of it stops a publish: a stale entry must
// not stop the whole mirror from being served.
type Report struct {
	// Tagged counts the components each ring tags.
	Tagged map[string]int
	// Dangling are versions the mirror does not hold: the ring serves nothing for
	// that component, so its hosts stay where they are.
	Dangling []Gap
	// Reissued are versions a ring serves of which the mirror holds several
	// releases: the newest is tagged, so a rebuild by the vendor replaces the one
	// the ring served until now.
	Reissued []Gap
	// Unknown are the IDs of entries whose component the mirror does not have.
	Unknown []string
	// Mismatched are the entries whose ID the mirror has but not their platform,
	// as a hand-written entry may get it wrong: no ring serves them.
	Mismatched []Entry
	// Overlaps are rings serving different versions through a GUID shared by two
	// components: fwupd merges the components matching a device, so hosts of one
	// can be moved to a release of the other.
	Overlaps []Overlap
	// OverlapCount is the total, as only the first are kept in Overlaps: vendors
	// share hundreds of GUIDs between platforms.
	OverlapCount int
	// Unmanaged counts the mirrored components with no entry; no ring serves them
	// until `bkc promote` adds them.
	Unmanaged int
}

// Apply tags the releases so that every ring serves what the file says. Tags are
// written into the releases of components, so a caller passes releases it owns.
func (f *File) Apply(components []lvfs.Component) *Report {
	report := &Report{Tagged: make(map[string]int)}
	entries := f.entries()
	seen := make(map[string]struct{}, len(entries))
	mirrored := make(map[string]struct{}, len(components))
	served := make(map[string]servedRelease)

	for i := range components {
		component := &components[i]
		mirrored[component.ID] = struct{}{}

		guids := lvfs.ComponentGUIDs(*component)
		key := entryKey(component.ID, digest(guids))
		entry, managed := entries[key]
		if !managed {
			report.Unmanaged++
			continue
		}
		seen[key] = struct{}{}

		for _, ring := range f.Rings {
			version := entry.Versions[ring]
			if version == "" {
				continue
			}
			index := releaseIndex(component.Releases, version)
			if index < 0 {
				report.Dangling = append(report.Dangling, Gap{Ring: ring, ComponentID: component.ID, Name: component.Name, Version: version})
				continue
			}
			component.Releases[index].AddTag(f.Tag(ring))
			report.Tagged[ring]++
			if countVersion(component.Releases, version) > 1 {
				report.Reissued = append(report.Reissued, Gap{Ring: ring, ComponentID: component.ID, Name: component.Name, Version: version})
			}
			for _, overlap := range overlaps(served, *component, guids, ring, component.Releases[index]) {
				report.OverlapCount++
				if len(report.Overlaps) < maxLogged {
					report.Overlaps = append(report.Overlaps, overlap)
				}
			}
		}
	}

	for _, entry := range f.Components {
		if _, ok := seen[entry.Key()]; ok {
			continue
		}
		if _, ok := mirrored[entry.ID]; ok {
			report.Mismatched = append(report.Mismatched, entry)
		} else {
			report.Unknown = append(report.Unknown, entry.ID)
		}
	}
	slices.Sort(report.Unknown)
	report.Unknown = slices.Compact(report.Unknown)
	return report
}

// LogTo logs the report as warnings, bounded so an incomplete file does not bury
// the rest of the log.
func (r *Report) LogTo(logger *slog.Logger) {
	for _, ring := range slices.Sorted(maps.Keys(r.Tagged)) {
		logger.Info("Tagged ring", "ring", ring, "components", r.Tagged[ring])
	}
	logLimited(logger, r.Dangling, len(r.Dangling), "Versions are not in the mirror, so their rings serve nothing for these components", func(gap Gap) []any {
		return []any{"ring", gap.Ring, "id", gap.ComponentID, "name", gap.Name, "version", gap.Version}
	})
	logLimited(logger, r.Reissued, len(r.Reissued), "The mirror holds several releases of a version a ring serves, and the newest is the one tagged", func(gap Gap) []any {
		return []any{"ring", gap.Ring, "id", gap.ComponentID, "name", gap.Name, "version", gap.Version}
	})
	logLimited(logger, r.Unknown, len(r.Unknown), "The definitions name components the mirror does not have", func(id string) []any {
		return []any{"id", id}
	})
	logLimited(logger, r.Overlaps, r.OverlapCount, "Two components sharing a GUID are served different releases by one ring, and fwupd can move hosts from one to the other", func(o Overlap) []any {
		return []any{"ring", o.Ring, "guid", o.GUID, "id", o.ComponentID, "version", o.Version, "other_id", o.OtherID, "other_version", o.OtherVersion}
	})
	logLimited(logger, r.Mismatched, len(r.Mismatched), "The mirror has these components, but not with the platform of their entry in the definitions, so no ring serves them", func(entry Entry) []any {
		return []any{"id", entry.ID, "platform", entry.Platform}
	})
	if r.Unmanaged > 0 {
		logger.Info("Mirrored components that no ring serves, as the definitions have no entry for them", "components", r.Unmanaged)
	}
}

// maxLogged bounds what LogTo logs of each kind: a stale file or shared GUIDs
// can make thousands, repeated by every refresh.
const maxLogged = 10

// logLimited warns of the first maxLogged items and counts the rest.
func logLimited[T any](logger *slog.Logger, items []T, total int, message string, attrs func(T) []any) {
	for i, item := range items {
		if i == maxLogged {
			break
		}
		logger.Warn(message, attrs(item)...)
	}
	if total > min(len(items), maxLogged) {
		logger.Warn(message, "more", total-min(len(items), maxLogged))
	}
}
