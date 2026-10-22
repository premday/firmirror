package lvfs

import (
	"maps"
	"slices"
	"strings"
)

// TagNamespace is the only tag namespace fwupd reads from remote metadata.
const TagNamespace = "lvfs"

// Tags is a component's tag list. Component holds it by pointer because the
// encoder emits the wrapper of an empty `tags>tag` slice: a plain slice would
// write <tags></tags> into every component.
type Tags struct {
	Tag []Tag `xml:"tag"`
}

type Tag struct {
	Namespace string `xml:"namespace,attr"`
	Value     string `xml:",chardata"`
}

// AddTag adds a tag, keeping Tags sorted and free of repeats. Releases are
// copied by value and share their tag slices, so it never edits them in place.
func (r *Release) AddTag(tag string) {
	if index, found := slices.BinarySearch(r.Tags, tag); !found {
		r.Tags = slices.Insert(slices.Clone(r.Tags), index, tag)
	}
}

// GroupByTags splits each component into one component per distinct tag set of
// its releases, as fwupd reads tags from the component. fwupd gathers the
// releases of every component matching a device's GUID, so splitting changes only
// the tags each release is served with. Pieces come untagged first, then by tag
// set.
func GroupByTags(components []Component) []Component {
	grouped := make([]Component, 0, len(components))
	for _, component := range components {
		if !slices.ContainsFunc(component.Releases, func(release Release) bool { return len(release.Tags) > 0 }) {
			// Nothing to split.
			component.Tags = nil
			grouped = append(grouped, component)
			continue
		}
		releases := make(map[string][]Release)
		for _, release := range component.Releases {
			key := strings.Join(release.Tags, "\n")
			releases[key] = append(releases[key], release)
		}

		for _, key := range slices.Sorted(maps.Keys(releases)) {
			piece := component
			piece.Releases = releases[key]
			piece.Tags = nil
			if key != "" {
				piece.Tags = &Tags{}
				for _, tag := range strings.Split(key, "\n") {
					piece.Tags.Tag = append(piece.Tags.Tag, Tag{Namespace: TagNamespace, Value: tag})
				}
			}
			grouped = append(grouped, piece)
		}
	}
	return grouped
}
