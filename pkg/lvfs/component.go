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
	guids := make([]string, 0, len(component.Provides))
	for _, provide := range component.Provides {
		guids = append(guids, provide.Text)
	}
	slices.Sort(guids)
	return component.ID + "\n" + strings.Join(slices.Compact(guids), ",")
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
