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

// heldIDs are the IDs that have a held entry. One without a platform stands for
// the components of its ID that have no entry of their own, and holds them all:
// the ID is held whole. One with a platform holds that component only, so another
// platform of the ID is served as any other is.
func (f *File) heldIDs() (whole, byPlatform map[string]struct{}) {
	whole, byPlatform = make(map[string]struct{}), make(map[string]struct{})
	for _, entry := range f.Components {
		switch {
		case !entry.Hold:
		case entry.HoldsWhole():
			whole[entry.ID] = struct{}{}
		default:
			byPlatform[entry.ID] = struct{}{}
		}
	}
	return whole, byPlatform
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

// Overlap is a pair of components that provide some of the same GUIDs and are
// served different releases by the rings it names: another version, or the same
// version of another package. fwupd resolves a device across the components
// providing its GUID, so it is given one of the two.
type Overlap struct {
	Rings []string
	// The components are told apart by ID and platform, as an ID has several.
	ComponentID, Platform                string
	Version                              string
	OtherID, OtherPlatform, OtherVersion string
	// Shared counts the GUIDs both components provide, Only and OtherOnly those
	// only one of them does. For the pairs that are logged, which are the first
	// that each have GUIDs of their own, OnlyGUIDs and OtherOnlyGUIDs name the
	// first of those: they tell which devices depend on which component.
	Shared, Only, OtherOnly   int
	OnlyGUIDs, OtherOnlyGUIDs []string
}

// Covered reports whether one component provides every GUID of the other. No
// device then depends on which of the two it is given beyond the version, unlike
// a pair that each have GUIDs of their own.
func (o Overlap) Covered() bool {
	return o.Only == 0 || o.OtherOnly == 0
}

// overlapBounds bound the pairing of the components, as vendors share hundreds of
// GUIDs between platforms and every pair of the components sharing one is a pair:
// at most pairs of them are kept per ring, and visits looked at in all, the rings
// together. A zero is the default.
type overlapBounds struct{ pairs, visits int }

const (
	defaultOverlapPairs  = 10000
	defaultOverlapVisits = 20_000_000
)

func (b overlapBounds) orDefault() overlapBounds {
	return overlapBounds{pairs: cmp.Or(b.pairs, defaultOverlapPairs), visits: cmp.Or(b.visits, defaultOverlapVisits)}
}

// maxGUIDsNamed bounds the GUIDs only one component of a pair provides that are
// named.
const maxGUIDsNamed = 10

// member is what overlaps needs to know of a mirrored component.
type member struct {
	id, platform string
	guids        []string
}

// servedRelease is what a ring serves of a component, by its index among the
// members; the location tells two packages of one version apart.
type servedRelease struct {
	component int
	releaseOf
}

// releaseOf is what tells the releases a ring serves apart.
type releaseOf struct{ version, location string }

// overlapKey identifies a pair of components and the releases a ring serves of
// them, so that the rings serving the same ones are reported together.
type overlapKey struct{ first, second servedRelease }

// overlaps pairs up the components a ring serves through a common GUID, and
// returns the pairs it serves different releases of. served is by ring, in
// the order of the members. Past its bounds it stops looking, which truncated
// says: what it returns is then not all, and the rings of a pair may not be.
func overlaps(members []member, rings []string, served map[string][]servedRelease, bounds overlapBounds) (found []Overlap, truncated bool) {
	pairs := make(map[overlapKey]*Overlap)
	visits := 0
	groupOf := make(map[releaseOf]int)
	for _, ring := range rings {
		byGUID := make(map[string][]int) // the components served through each GUID, in order
		release := make([]servedRelease, len(members))
		for _, one := range served[ring] {
			release[one.component] = one
			for _, guid := range members[one.component].guids {
				byGUID[guid] = append(byGUID[guid], one.component)
			}
		}
		shared := make(map[[2]int]struct{})
		// Only a GUID that several components are served through makes a pair, and in
		// order, so that what is dropped past the bounds is the same every time.
		var common []string
		for guid, releases := range byGUID {
			if len(releases) > 1 {
				common = append(common, guid)
			}
		}
		slices.Sort(common)
	guids:
		for _, guid := range common {
			// The components served the same release are no pair: group them, in the
			// order they come, and pair the groups, so that every visit is a pair.
			var groups [][]int
			clear(groupOf)
			for _, component := range byGUID[guid] {
				key := release[component].releaseOf
				group, ok := groupOf[key]
				if !ok {
					group = len(groups)
					groupOf[key] = group
					groups = append(groups, nil)
				}
				groups[group] = append(groups[group], component)
			}
			for i, one := range groups {
				for _, other := range groups[i+1:] {
					for _, a := range one {
						for _, b := range other {
							if visits++; visits > bounds.visits {
								truncated = true
								break guids
							}
							pair := [2]int{min(a, b), max(a, b)}
							if _, known := shared[pair]; known {
								continue
							}
							if len(shared) >= bounds.pairs {
								truncated = true
								break guids
							}
							shared[pair] = struct{}{}
						}
					}
				}
			}
		}
		for pair := range shared {
			key := overlapKey{release[pair[0]], release[pair[1]]}
			overlap, ok := pairs[key]
			if !ok {
				overlap = newOverlap(members, key)
				pairs[key] = overlap
			}
			overlap.Rings = append(overlap.Rings, ring)
		}
	}

	found = make([]Overlap, 0, len(pairs))
	for _, overlap := range pairs {
		found = append(found, *overlap)
	}
	slices.SortFunc(found, compareOverlaps)
	// Only the pairs that are logged need their GUIDs named: the first maxLogged
	// that each have GUIDs of their own, as LogTo logs them. The members are
	// unique by ID and platform, as the components are pooled by key.
	guidsOf := func(id, platform string) []string {
		for _, one := range members {
			if one.id == id && one.platform == platform {
				return one.guids
			}
		}
		return nil
	}
	named := 0
	for i := range found {
		if overlap := &found[i]; !overlap.Covered() && named < maxLogged {
			first, second := guidsOf(overlap.ComponentID, overlap.Platform), guidsOf(overlap.OtherID, overlap.OtherPlatform)
			overlap.OnlyGUIDs = firstNotIn(first, second, maxGUIDsNamed)
			overlap.OtherOnlyGUIDs = firstNotIn(second, first, maxGUIDsNamed)
			named++
		}
	}
	return found, truncated
}

// compareOverlaps orders the pairs by their components, then their releases, then
// their rings, so that no two compare equal unless they say the same.
func compareOverlaps(a, b Overlap) int {
	return cmp.Or(
		strings.Compare(a.ComponentID, b.ComponentID),
		strings.Compare(a.OtherID, b.OtherID),
		strings.Compare(a.Platform, b.Platform),
		strings.Compare(a.OtherPlatform, b.OtherPlatform),
		strings.Compare(a.Version, b.Version),
		strings.Compare(a.OtherVersion, b.OtherVersion),
		slices.Compare(a.Rings, b.Rings),
	)
}

func newOverlap(members []member, key overlapKey) *Overlap {
	first, second := members[key.first.component], members[key.second.component]
	shared := sharedGUIDs(first.guids, second.guids)
	return &Overlap{
		ComponentID: first.id, Platform: first.platform, Version: key.first.version,
		OtherID: second.id, OtherPlatform: second.platform, OtherVersion: key.second.version,
		Shared: shared, Only: len(first.guids) - shared, OtherOnly: len(second.guids) - shared,
	}
}

// sharedGUIDs counts the GUIDs two sorted lists have in common.
func sharedGUIDs(a, b []string) int {
	shared := 0
	for i, j := 0, 0; i < len(a) && j < len(b); {
		switch c := strings.Compare(a[i], b[j]); {
		case c < 0:
			i++
		case c > 0:
			j++
		default:
			shared++
			i++
			j++
		}
	}
	return shared
}

// firstNotIn returns the first limit of the GUIDs that are not in others. Both
// are sorted, as lvfs.ComponentGUIDs returns them.
func firstNotIn(guids, others []string, limit int) []string {
	var found []string
	j := 0
	for _, guid := range guids {
		if len(found) == limit {
			break
		}
		for j < len(others) && others[j] < guid {
			j++
		}
		if j == len(others) || others[j] != guid {
			found = append(found, guid)
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
	// OverlapsTruncated says Overlaps lacks pairs: there are more than were looked at
	// or kept, as the pairing is bounded.
	OverlapsTruncated bool
	// Overlaps are the pairs of components that share GUIDs and are served
	// different releases: fwupd merges the components matching a device, so hosts
	// of one can be moved to a release of the other. Vendors share hundreds of
	// GUIDs between platforms, so a pair is one entry, whatever the GUIDs.
	Overlaps []Overlap
	// Held counts the mirrored components that are held out of every ring: those
	// of a held entry, and those with no entry of an ID whose held entry has no
	// platform.
	Held int
	// Unmanaged counts the mirrored components with no entry, and no held entry
	// without a platform for their ID; no ring serves them until `bkc promote`
	// adds them.
	Unmanaged int
}

// Apply tags the releases so that every ring serves what the file says. Tags are
// written into the releases of components, so a caller passes releases it owns.
func (f *File) Apply(components []lvfs.Component) *Report {
	return f.apply(components, overlapBounds{})
}

// apply is Apply, pairing the components by the bounds given, which tests set.
func (f *File) apply(components []lvfs.Component, bounds overlapBounds) *Report {
	report := &Report{Tagged: make(map[string]int)}
	entries := f.entries()
	heldIDs, _ := f.heldIDs()
	seen := make(map[string]struct{}, len(entries))
	mirrored := make(map[string]struct{}, len(components))
	served := make(map[string][]servedRelease)
	members := make([]member, len(components))

	for i := range components {
		component := &components[i]
		mirrored[component.ID] = struct{}{}

		guids := lvfs.ComponentGUIDs(*component)
		members[i] = member{id: component.ID, platform: digest(guids), guids: guids}
		key := entryKey(component.ID, members[i].platform)
		entry, managed := entries[key]
		if !managed {
			if _, held := heldIDs[component.ID]; held {
				report.Held++
			} else {
				report.Unmanaged++
			}
			continue
		}
		seen[key] = struct{}{}
		if entry.Hold {
			report.Held++
		}

		for _, ring := range f.Rings {
			version := entry.Served(ring)
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
			served[ring] = append(served[ring], servedRelease{component: i, releaseOf: releaseOf{version, component.Releases[index].Location}})
		}
	}
	report.Overlaps, report.OverlapsTruncated = overlaps(members, f.Rings, served, bounds.orDefault())

	for _, entry := range f.Components {
		if _, ok := seen[entry.Key()]; ok {
			continue
		}
		if _, ok := mirrored[entry.ID]; ok {
			// A held entry without a platform holds its ID whole, whatever the
			// components of it that have an entry. One with a platform that the mirror
			// lacks may be a package that changed it.
			if !entry.HoldsWhole() {
				report.Mismatched = append(report.Mismatched, entry)
			}
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
	partial, covered := splitOverlaps(r.Overlaps)
	logLimited(logger, partial, len(partial), "Two components share GUIDs and each has GUIDs of its own, and they are served different releases, so a host is given one or the other by the GUIDs it matches", overlapAttrs)
	logLimited(logger, covered, len(covered), "One component provides every GUID of another, and they are served different releases, so a host matching both is given one of them", overlapAttrs)
	if r.OverlapsTruncated {
		logger.Warn("There are more pairs of components served different releases through a GUID than were looked at or kept, so those listed are not all, and the rings of a pair may not be", "listed", len(r.Overlaps))
	}
	logLimited(logger, r.Mismatched, len(r.Mismatched), "The mirror has these components, but not with the platform of their entry in the definitions, so no ring serves them", func(entry Entry) []any {
		attrs := []any{"id", entry.ID, "platform", entry.Platform}
		if entry.Hold {
			// The package it holds may have changed its platform.
			attrs = append(attrs, "hold", true)
		}
		return attrs
	})
	if r.Held > 0 {
		logger.Info("Components held out of every ring, as the definitions say", "components", r.Held)
	}
	if r.Unmanaged > 0 {
		logger.Info("Mirrored components that no ring serves, as the definitions have no entry for them", "components", r.Unmanaged)
	}
}

// splitOverlaps separates the pairs that each have GUIDs of their own from those
// where one component covers the other, keeping their order.
func splitOverlaps(all []Overlap) (partial, covered []Overlap) {
	for _, overlap := range all {
		if overlap.Covered() {
			covered = append(covered, overlap)
		} else {
			partial = append(partial, overlap)
		}
	}
	return partial, covered
}

func overlapAttrs(o Overlap) []any {
	attrs := []any{
		"rings", strings.Join(o.Rings, ","),
		"id", o.ComponentID, "platform", o.Platform, "version", o.Version,
		"other_id", o.OtherID, "other_platform", o.OtherPlatform, "other_version", o.OtherVersion,
		"shared", o.Shared, "only", o.Only, "other_only", o.OtherOnly,
	}
	if len(o.OnlyGUIDs) > 0 {
		attrs = append(attrs, "only_guids", guidList(o.OnlyGUIDs, o.Only), "other_only_guids", guidList(o.OtherOnlyGUIDs, o.OtherOnly))
	}
	return attrs
}

// guidList joins the GUIDs kept of total, saying when it is not all of them.
func guidList(kept []string, total int) string {
	list := strings.Join(kept, ",")
	if total > len(kept) {
		list += ",..."
	}
	return list
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
