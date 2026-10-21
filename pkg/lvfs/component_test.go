package lvfs

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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

func TestPoolByKeyNeverWritesIntoWhatItWasGiven(t *testing.T) {
	spare := make([]Release, 1, 8)
	spare[0] = Release{Version: "1"}
	first := Component{ID: "a", Releases: spare}
	second := Component{ID: "a", Releases: []Release{{Version: "2"}}}
	alone := Component{ID: "b", Releases: []Release{{Version: "3"}}}

	pooled := PoolByKey([]Component{first, second, alone})

	require.Len(t, pooled, 2)
	assert.Len(t, pooled[0].Releases, 2)
	assert.Equal(t, 1, len(spare))
	assert.Equal(t, Release{}, spare[:2][1], "the spare capacity of the given slice is not written to")
	assert.Same(t, &alone.Releases[0], &pooled[1].Releases[0], "a component published in one piece is not copied")
}

func TestComponentGUIDs(t *testing.T) {
	component := Component{Provides: []Firmware{{Text: "b"}, {Text: "a"}, {Text: "b"}}}

	assert.Equal(t, []string{"a", "b"}, ComponentGUIDs(component))
	assert.Equal(t, "\na,b", ComponentKey(component))
}

func TestAnEmptyGUIDMakesNoComponentOfItsOwn(t *testing.T) {
	with := Component{ID: "a", Provides: []Firmware{{Text: ""}, {Text: "g"}}}
	without := Component{ID: "a", Provides: []Firmware{{Text: "g"}}}

	assert.Equal(t, ComponentKey(without), ComponentKey(with))
}
