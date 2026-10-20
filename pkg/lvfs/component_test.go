package lvfs

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestPoolByKey(t *testing.T) {
	piece := func(id, guid string, versions ...string) Component {
		component := Component{ID: id, Provides: []Firmware{{Type: "flashed", Text: guid}}}
		for _, version := range versions {
			component.Releases = append(component.Releases, Release{Version: version})
		}
		return component
	}
	first := piece("a", "g1", "1")
	// Room to grow, as a decoded document leaves its slices.
	first.Releases = append(make([]Release, 0, 4), first.Releases...)
	original := []Component{
		first,
		piece("b", "g1", "9"),
		piece("a", "g1", "2", "3"),
		piece("a", "g2", "7"),
	}

	pooled := PoolByKey(original)

	assert.Len(t, pooled, 3, "same ID and GUIDs is one component, another platform is another")
	assert.Equal(t, "a", pooled[0].ID)
	assert.Equal(t, []string{"1", "2", "3"}, []string{pooled[0].Releases[0].Version, pooled[0].Releases[1].Version, pooled[0].Releases[2].Version})
	assert.Equal(t, "b", pooled[1].ID)
	assert.Equal(t, "g2", pooled[2].Provides[0].Text)
	assert.Len(t, original[0].Releases, 1)
	assert.Empty(t, original[0].Releases[:cap(original[0].Releases)][1].Version, "the pieces it was given are not written into")
}
