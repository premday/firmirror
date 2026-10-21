package lvfs

import (
	"slices"
	"strings"
)

// ComponentKey identifies a component by its ID and its GUIDs. One set of GUIDs
// covers all of a component's releases, so a package covering other platforms
// must not merge its releases in: fwupd would reject the cabinet it downloads
// with "No supported devices found".
func ComponentKey(component Component) string {
	return component.ID + "\n" + strings.Join(ComponentGUIDs(component), ",")
}

// ComponentGUIDs returns the sorted, distinct GUIDs a component provides. An empty
// one names no device and is left out.
func ComponentGUIDs(component Component) []string {
	guids := make([]string, 0, len(component.Provides))
	for _, provide := range component.Provides {
		guids = append(guids, provide.Text)
	}
	return distinct(guids)
}

// DistinctGUIDs returns a sorted copy of guids without repeats or empty values,
// so every comparison of GUID sets agrees on what the same set is.
func DistinctGUIDs(guids []string) []string {
	return distinct(slices.Clone(guids))
}

// distinct is DistinctGUIDs, reordering guids in place.
func distinct(guids []string) []string {
	slices.Sort(guids)
	return slices.DeleteFunc(slices.Compact(guids), func(guid string) bool { return guid == "" })
}

// PoolByKey puts together the pieces a component was published in, keeping the
// order of their first appearance. The releases are copied, so appending to a
// pooled component never writes into the slices it was given.
func PoolByKey(components []Component) []Component {
	pooled := make([]Component, 0, len(components))
	index := make(map[string]int, len(components))
	for _, component := range components {
		key := ComponentKey(component)
		if at, ok := index[key]; ok {
			pooled[at].Releases = append(pooled[at].Releases, component.Releases...)
			continue
		}
		index[key] = len(pooled)
		component.Releases = slices.Clone(component.Releases)
		pooled = append(pooled, component)
	}
	return pooled
}
