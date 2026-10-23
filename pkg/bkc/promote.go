package bkc

import (
	"fmt"
	"log/slog"
	"slices"
	"strings"

	"github.com/premday/firmirror/pkg/lvfs"
)

// Change is one component a promotion moves.
type Change struct {
	ComponentID string
	// Platform is the entry's, which tells apart the platforms of one ID.
	Platform string
	Name     string
	// From is empty when the ring served nothing.
	From string
	To   string
	// Downgrade is set when the change moves the ring back to an older release,
	// which downgrades its hosts: how a ring is rolled back.
	Downgrade bool
	// Withdrawn is set when the version the ring served is no longer in the
	// mirror, so a downgrade is assumed and not known.
	Withdrawn bool
}

// Note describes the change in the table of a promotion, besides its versions.
func (c Change) Note() string {
	switch {
	case c.Withdrawn && c.Downgrade:
		return "downgrade, from a version the mirror no longer has"
	case c.Withdrawn:
		return "from a version the mirror no longer has"
	case c.Downgrade:
		return "downgrade"
	}
	return ""
}

// CountDowngrades counts the changes that move a ring backwards.
func CountDowngrades(changes []Change) int {
	count := 0
	for _, change := range changes {
		if change.Downgrade {
			count++
		}
	}
	return count
}

// movesBackwards reports whether moving a ring from one version to another goes
// back to an older release. When from is no longer in the mirror it cannot be
// placed, and withdrawn is set: a move to the newest release is forward, any
// other counts as backwards, as hosts may still run the withdrawn version.
func movesBackwards(releases []lvfs.Release, to, from string) (backwards, withdrawn bool) {
	if from == "" {
		return false, false
	}
	first, second := releaseIndex(releases, to), releaseIndex(releases, from)
	switch {
	case first < 0:
		return false, false
	case second < 0:
		newest := lvfs.NewestRelease(releases)
		return newest == nil || newest.Version != to, true
	}
	// Without a date nothing tells which release is newer, so a move off or to an
	// undated release is assumed backwards.
	if undated(releases[second]) || undated(releases[first]) {
		return true, false
	}
	return lvfs.Newer(releases[second], releases[first]), false
}

// undated reports whether a release has no date, as HPE publishes when neither
// the package nor the catalog has one. It sorts as the oldest.
func undated(release lvfs.Release) bool {
	return release.Date == "" || strings.HasPrefix(release.Date, "0001-01-01")
}

// Promote returns the file with a ring promoted, and the changes. The first ring
// takes the newest release of every mirrored component, adding entries as needed.
// Any other ring takes what the ring before it serves; a version the mirror lost
// is skipped with a warning. An entry without a platform is given the one of the
// mirrored component of its ID, when there is only one. components are the whole
// mirrored ones (see lvfs.PoolByKey); only, when set, narrows the promotion to
// those component IDs, which must all be mirrored.
func (f *File) Promote(components []lvfs.Component, ring string, only []string) (*File, []Change, error) {
	upstream, err := f.Upstream(ring)
	if err != nil {
		return nil, nil, err
	}
	for _, id := range only {
		if !slices.ContainsFunc(components, func(component lvfs.Component) bool { return component.ID == id }) {
			return nil, nil, fmt.Errorf("component %q is not in the mirror", id)
		}
	}

	promoted := f.Clone()
	index := make(map[string]int, len(promoted.Components))
	for i, entry := range promoted.Components {
		index[entry.Key()] = i
	}
	bare := newBareEntries(promoted.Components, components)
	var dates undatedNotes

	var unversioned []string
	var lost []Gap
	var changes []Change
	for _, component := range components {
		if len(only) > 0 && !slices.Contains(only, component.ID) {
			continue
		}
		platform := platformOf(component)
		key := entryKey(component.ID, platform)
		at, known := index[key]
		if !known && platform != "" {
			adopted, ok, err := bare.adopt(component.ID)
			if err != nil {
				return nil, nil, err
			}
			if ok {
				promoted.Components[adopted].Platform = platform
				index[key], at, known = adopted, adopted, true
			}
		}

		target := ""
		if upstream == "" {
			if newest := lvfs.NewestRelease(component.Releases); newest != nil {
				target = newest.Version
				dates.note(component)
			}
		} else if known {
			target = promoted.Components[at].Versions[upstream]
			if target != "" && releaseIndex(component.Releases, target) < 0 {
				lost = append(lost, Gap{Ring: upstream, ComponentID: component.ID, Name: component.Name, Version: target})
				continue
			}
		}
		if target == "" {
			if upstream == "" && len(component.Releases) > 0 {
				unversioned = append(unversioned, component.ID)
			}
			continue
		}

		if !known {
			at = len(promoted.Components)
			index[key] = at
			promoted.Components = append(promoted.Components, Entry{ID: component.ID, Platform: platform})
		}
		entry := &promoted.Components[at]
		if entry.Versions == nil {
			entry.Versions = make(map[string]string)
		}
		current := entry.Versions[ring]
		if current == target {
			continue
		}
		// The name follows the vendor's only when a version moves, to keep the diff
		// to what the table reports.
		entry.Name = OneLine(component.Name)
		entry.Versions[ring] = target
		downgrade, withdrawn := movesBackwards(component.Releases, target, current)
		changes = append(changes, Change{
			ComponentID: component.ID, Platform: platform, Name: component.Name, From: current, To: target,
			Downgrade: downgrade, Withdrawn: withdrawn,
		})
	}
	dates.warn()
	logLimited(slog.Default(), lost, len(lost), "Version is not in the mirror, so it is not promoted into "+ring, func(gap Gap) []any {
		return []any{"from", gap.Ring, "id", gap.ComponentID, "name", gap.Name, "version", gap.Version}
	})
	if len(unversioned) > 0 {
		slog.Warn("Some components have a newest release that names no version, so they are not promoted: pin a version in the file if one is",
			"components", len(unversioned), "first", unversioned[:min(len(unversioned), 5)])
	}
	return promoted, changes, nil
}

// claims returns the keys of the mirrored components, and by ID the platforms of
// those no entry is about (what an entry without a platform may stand for).
func claims(entries []Entry, components []lvfs.Component) (mirrored map[string]struct{}, unclaimed map[string][]string) {
	listed := make(map[string]struct{}, len(entries))
	for _, entry := range entries {
		listed[entry.Key()] = struct{}{}
	}
	mirrored = make(map[string]struct{}, len(components))
	unclaimed = make(map[string][]string)
	for _, component := range components {
		key := keyOf(component)
		mirrored[key] = struct{}{}
		if _, ok := listed[key]; !ok {
			unclaimed[component.ID] = append(unclaimed[component.ID], platformOf(component))
		}
	}
	return mirrored, unclaimed
}

// bareEntries are the entries without a platform that match no mirrored
// component as they are, and so can be given the platform of one.
type bareEntries struct {
	at        map[string]int      // by component ID
	unclaimed map[string][]string // the platforms of the mirrored components no entry is about, by ID
}

func newBareEntries(entries []Entry, components []lvfs.Component) *bareEntries {
	mirrored, unclaimed := claims(entries, components)
	bare := &bareEntries{at: make(map[string]int), unclaimed: unclaimed}
	for i, entry := range entries {
		if _, claimed := mirrored[entry.Key()]; entry.Platform == "" && !claimed {
			bare.at[entry.ID] = i
		}
	}
	return bare
}

// adopt returns the platformless entry for id, which then belongs to that
// component alone. It fails when several components of the ID have no entry, as
// it cannot tell which the entry is about, and names their platforms.
func (b *bareEntries) adopt(id string) (at int, ok bool, err error) {
	at, ok = b.at[id]
	if !ok {
		return 0, false, nil
	}
	if platforms := b.unclaimed[id]; len(platforms) != 1 {
		return 0, false, fmt.Errorf("the entry of component %q has no platform, and %d platforms of it have no entry: set the platform of the entry to one of %s",
			id, len(platforms), strings.Join(platforms, ", "))
	}
	delete(b.at, id)
	return at, true, nil
}

// undatedNotes collects the components with undated releases, to warn once.
type undatedNotes struct{ some, all []string }

func (u *undatedNotes) note(component lvfs.Component) {
	switch {
	case !slices.ContainsFunc(component.Releases, undated):
	case slices.ContainsFunc(component.Releases, func(release lvfs.Release) bool { return !undated(release) }):
		u.some = append(u.some, component.ID)
	default:
		u.all = append(u.all, component.ID)
	}
}

func (u *undatedNotes) warn() {
	if len(u.some) > 0 {
		slog.Warn("Some components have releases with no date, which are never taken as the newest: pin the version in the file if one is",
			"components", len(u.some), "first", u.some[:min(len(u.some), 5)])
	}
	if len(u.all) > 0 {
		slog.Warn("Some components have no release with a date, so what is taken as the newest is told by timestamp, checksum and location, which mean nothing: pin the version in the file",
			"components", len(u.all), "first", u.all[:min(len(u.all), 5)])
	}
}

// Prune returns the file without the entries whose component the mirror no
// longer has, and those entries. A platformless entry stands for the mirrored
// components of its ID that no other entry is about, and goes when there are
// none. components are the mirrored ones, as for Promote.
func (f *File) Prune(components []lvfs.Component) (*File, []Entry) {
	mirrored, unclaimed := claims(f.Components, components)
	pruned := f.Clone()
	var removed []Entry
	pruned.Components = slices.DeleteFunc(pruned.Components, func(entry Entry) bool {
		_, kept := mirrored[entry.Key()]
		if entry.Platform == "" && len(unclaimed[entry.ID]) > 0 {
			kept = true
		}
		if !kept {
			removed = append(removed, entry)
		}
		return !kept
	})
	return pruned, removed
}
