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
