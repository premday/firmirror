package bkc

import (
	"testing"

	"github.com/premday/firmirror/pkg/lvfs"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func mirror() []lvfs.Component {
	return []lvfs.Component{
		component("a", release("1.0", "2026-01-01"), release("2.0", "2026-02-01"), release("3.0", "2026-03-01")),
		component("b", release("1.0", "2026-01-01")),
	}
}

func TestPromoteDevTakesTheNewestAndCreatesEntries(t *testing.T) {
	file := New(DefaultRings)

	promoted, changes, err := file.Promote(mirror(), "dev", nil)

	require.NoError(t, err)
	assert.Equal(t, []Change{
		{ComponentID: "a", Name: "Name of a", To: "3.0"},
		{ComponentID: "b", Name: "Name of b", To: "1.0"},
	}, changes)
	assert.Equal(t, "3.0", promoted.Components[0].Versions["dev"])
	assert.Empty(t, file.Components, "the file it was given is not edited")
}

func TestPromoteDevMovesOnWhatNewerFirmwareWasMirrored(t *testing.T) {
	file := testFile(t, Entry{ID: "a", Versions: map[string]string{"dev": "2.0", "beta": "1.0"}})

	promoted, changes, err := file.Promote(mirror(), "dev", []string{"a"})

	require.NoError(t, err)
	require.Len(t, changes, 1)
	assert.Equal(t, "2.0", changes[0].From)
	assert.Equal(t, "3.0", changes[0].To)
	assert.Equal(t, map[string]string{"dev": "3.0", "beta": "1.0"}, promoted.Components[0].Versions, "the other rings stay where they were")
	assert.Len(t, promoted.Components, 1, "b is not among the components asked for")
}

func TestPromoteTakesWhatTheRingBeforeServes(t *testing.T) {
	file := testFile(t,
		Entry{ID: "a", Versions: map[string]string{"dev": "3.0", "beta": "2.0"}},
		Entry{ID: "b", Versions: map[string]string{"dev": "1.0"}},
	)

	promoted, changes, err := file.Promote(mirror(), "preview", nil)

	require.NoError(t, err)
	require.Len(t, changes, 1, "b is served by no beta, so there is nothing to take")
	assert.Equal(t, Change{ComponentID: "a", Name: "Name of a", To: "2.0"}, changes[0])
	assert.Equal(t, "2.0", promoted.Components[0].Versions["preview"])
	assert.Empty(t, promoted.Components[1].Versions["preview"])
}

func TestPromoteSkipsWhatIsAlreadyThere(t *testing.T) {
	file := testFile(t, Entry{ID: "a", Versions: map[string]string{"dev": "3.0", "beta": "3.0"}})

	_, changes, err := file.Promote(mirror(), "beta", nil)

	require.NoError(t, err)
	assert.Empty(t, changes)
}

func TestPromoteFlagsARingMovingBack(t *testing.T) {
	file := testFile(t, Entry{ID: "a", Versions: map[string]string{"dev": "1.0", "beta": "3.0"}})

	_, changes, err := file.Promote(mirror(), "beta", nil)

	require.NoError(t, err)
	require.Len(t, changes, 1)
	assert.True(t, changes[0].Downgrade, "rolling a ring back is promoting it from a ring that is behind")
	assert.Equal(t, 1, CountDowngrades(changes))
}

func TestPromoteOnlyAndErrors(t *testing.T) {
	file := New(DefaultRings)

	_, _, err := file.Promote(mirror(), "dev", []string{"nowhere"})
	assert.ErrorContains(t, err, `"nowhere" is not in the mirror`)
	_, _, err = file.Promote(mirror(), "nowhere", nil)
	assert.ErrorContains(t, err, "unknown ring")
	_, changes, err := file.Promote(mirror(), "beta", nil)
	require.NoError(t, err, "a ring after dev with nothing in dev has nothing to take")
	assert.Empty(t, changes)
}

func TestPromoteKeepsPlatformsApart(t *testing.T) {
	file := New(DefaultRings)
	components := []lvfs.Component{
		platform("a", "g1", release("1.0", "2026-01-01")),
		platform("a", "g2", release("2.0", "2026-02-01")),
	}

	promoted, _, err := file.Promote(components, "dev", nil)

	require.NoError(t, err)
	require.Len(t, promoted.Components, 2)
	assert.Equal(t, Platform([]string{"g1"}), promoted.Components[0].Platform)
	assert.Equal(t, "1.0", promoted.Components[0].Versions["dev"])
	assert.Equal(t, "2.0", promoted.Components[1].Versions["dev"])
}

func TestPromoteSkipsAVersionTheMirrorLost(t *testing.T) {
	file := testFile(t, Entry{ID: "a", Versions: map[string]string{"dev": "9.9"}})

	promoted, changes, err := file.Promote(mirror(), "beta", nil)

	require.NoError(t, err)
	assert.Empty(t, changes, "no ring could serve it")
	assert.Empty(t, promoted.Components[0].Versions["beta"])
}

func TestPromoteFillsAnEntryWithNoVersions(t *testing.T) {
	file := New(DefaultRings)
	file.Components = []Entry{{ID: "a"}}
	require.NoError(t, file.Validate())

	promoted, changes, err := file.Promote(mirror(), "dev", []string{"a"})

	require.NoError(t, err)
	require.Len(t, changes, 1)
	assert.Equal(t, "3.0", promoted.Components[0].Versions["dev"])
}

func TestPromoteGivesAnEntryWithNoPlatformTheMirrorsOwn(t *testing.T) {
	file := testFile(t, Entry{ID: "a", Versions: map[string]string{"dev": "1.0"}})
	components := []lvfs.Component{platform("a", "g1", release("1.0", "2026-01-01"), release("2.0", "2026-02-01"))}

	promoted, changes, err := file.Promote(components, "dev", nil)

	require.NoError(t, err)
	require.Len(t, promoted.Components, 1, "no second entry for the component")
	assert.Equal(t, Platform([]string{"g1"}), promoted.Components[0].Platform)
	assert.Equal(t, "2.0", promoted.Components[0].Versions["dev"])
	require.Len(t, changes, 1)
	assert.Equal(t, "1.0", changes[0].From)
	assert.Empty(t, file.Components[0].Platform, "the file it was given is not edited")
}

func TestPromoteRefusesToGuessThePlatformOfAnEntry(t *testing.T) {
	file := testFile(t, Entry{ID: "a", Versions: map[string]string{"dev": "1.0"}})
	components := []lvfs.Component{
		platform("a", "g1", release("1.0", "2026-01-01")),
		platform("a", "g2", release("1.0", "2026-01-01")),
	}

	_, _, err := file.Promote(components, "dev", nil)

	assert.ErrorContains(t, err, "set the platform of the entry to one of "+Platform([]string{"g1"})+", "+Platform([]string{"g2"}))
}

func TestPromoteCompletesAnEntryWhoseVersionDidNotMove(t *testing.T) {
	file := testFile(t, Entry{ID: "a", Versions: map[string]string{"dev": "2.0"}})
	components := []lvfs.Component{platform("a", "g1", release("1.0", "2026-01-01"), release("2.0", "2026-02-01"))}

	promoted, changes, err := file.Promote(components, "dev", nil)

	require.NoError(t, err)
	assert.Empty(t, changes)
	assert.Equal(t, Platform([]string{"g1"}), promoted.Components[0].Platform)
	assert.False(t, promoted.Equal(file), "a caller has to be able to tell that there is something to write")
}

func TestPromoteFlagsALeaveFromAVersionTheMirrorLost(t *testing.T) {
	file := testFile(t, Entry{ID: "a", Versions: map[string]string{"dev": "1.0", "beta": "9.9"}})

	_, changes, err := file.Promote(mirror(), "beta", nil)

	require.NoError(t, err)
	require.Len(t, changes, 1)
	assert.True(t, changes[0].Downgrade, "hosts may still run it, and nothing says the move is forward")
	assert.True(t, changes[0].Withdrawn)
}

func TestPrune(t *testing.T) {
	g := Platform([]string{"g1"})
	file := testFile(t,
		Entry{ID: "a", Versions: map[string]string{"dev": "1.0"}},
		Entry{ID: "gone", Versions: map[string]string{"dev": "1.0"}},
		Entry{ID: "b", Platform: g, Versions: map[string]string{"dev": "1.0"}},
		Entry{ID: "b", Platform: "0123456789abcdef", Versions: map[string]string{"dev": "1.0"}},
	)
	components := []lvfs.Component{component("a"), platform("b", "g1")}

	pruned, removed := file.Prune(components)

	require.Len(t, removed, 2)
	assert.Equal(t, "gone", removed[0].ID)
	assert.Equal(t, "0123456789abcdef", removed[1].Platform)
	require.Len(t, pruned.Components, 2)
	var left []string
	for _, entry := range file.Components {
		left = append(left, entry.ID+"/"+entry.Platform)
	}
	assert.Equal(t, []string{"a/", "gone/", "b/" + g, "b/0123456789abcdef"}, left, "the file it was given is not edited")
}

func TestPromoteDoesNotGiveAPlatformToAnEntryThatAlreadyHasOne(t *testing.T) {
	stale := Platform([]string{"gone"})
	file := testFile(t, Entry{ID: "a", Platform: stale, Versions: map[string]string{"dev": "1.0"}})

	promoted, _, err := file.Promote([]lvfs.Component{platform("a", "g1", release("2.0", "2026-02-01"))}, "dev", nil)

	require.NoError(t, err)
	require.Len(t, promoted.Components, 2)
	assert.Equal(t, stale, promoted.Components[0].Platform, "an entry about another platform is not the mirrored one's")
	assert.Equal(t, "1.0", promoted.Components[0].Versions["dev"])
	assert.Equal(t, Platform([]string{"g1"}), promoted.Components[1].Platform)
}

func TestPromoteFromAWithdrawnVersionToTheNewestIsForward(t *testing.T) {
	file := testFile(t, Entry{ID: "a", Versions: map[string]string{"dev": "9.9"}})

	_, changes, err := file.Promote(mirror(), "dev", []string{"a"})

	require.NoError(t, err)
	require.Len(t, changes, 1)
	assert.Equal(t, "3.0", changes[0].To)
	assert.True(t, changes[0].Withdrawn, "it is said, so that the table can say it")
	assert.False(t, changes[0].Downgrade, "nothing is newer than the newest release, so a real downgrade is not hidden behind this one")
}

func TestPruneDropsAnEntryWithNoPlatformOnceThePlatformsHaveTheirOwn(t *testing.T) {
	g1, g2 := Platform([]string{"g1"}), Platform([]string{"g2"})
	components := []lvfs.Component{platform("a", "g1"), platform("a", "g2")}

	stale := testFile(t,
		Entry{ID: "a", Versions: map[string]string{"dev": "1.0"}},
		Entry{ID: "a", Platform: g1, Versions: map[string]string{"dev": "1.0"}},
		Entry{ID: "a", Platform: g2, Versions: map[string]string{"dev": "1.0"}},
	)
	pruned, removed := stale.Prune(components)
	require.Len(t, removed, 1)
	assert.Empty(t, removed[0].Platform)
	assert.Len(t, pruned.Components, 2)

	pending := testFile(t,
		Entry{ID: "a", Versions: map[string]string{"dev": "1.0"}},
		Entry{ID: "a", Platform: g1, Versions: map[string]string{"dev": "1.0"}},
	)
	_, removed = pending.Prune(components)
	assert.Empty(t, removed, "g2 has no entry yet, and the one with no platform may be about it")
}

func TestPromoteKeepsAnEntryOfAComponentWithNoGUIDsAndAdoptsNoOtherForIt(t *testing.T) {
	file := testFile(t, Entry{ID: "a", Versions: map[string]string{"dev": "1.0"}})
	components := []lvfs.Component{
		component("a", release("2.0", "2026-02-01")),
		platform("a", "g1", release("5.0", "2026-03-01")),
	}

	promoted, _, err := file.Promote(components, "dev", nil)

	require.NoError(t, err)
	require.Len(t, promoted.Components, 2)
	assert.Equal(t, "2.0", promoted.Components[0].Versions["dev"], "the entry stays about the component with no GUIDs")
	assert.Equal(t, "5.0", promoted.Components[1].Versions["dev"])
}

func TestPromoteLeavesTheFileAloneWhenOnlyAVendorNameChanged(t *testing.T) {
	file := testFile(t, Entry{ID: "a", Name: "Old name", Versions: map[string]string{"dev": "3.0"}})

	promoted, changes, err := file.Promote(mirror(), "dev", []string{"a"})

	require.NoError(t, err)
	assert.Empty(t, changes)
	assert.True(t, promoted.Equal(file), "a rewrite the table does not announce is not made")
}

func TestPromoteDoesNotQuietlyMoveAPinnedUndatedVersionToADatedOne(t *testing.T) {
	file := testFile(t, Entry{ID: "a", Versions: map[string]string{"dev": "2.0"}})
	components := []lvfs.Component{component("a", release("1.0", "2024-01-01"), release("2.0", "0001-01-01"))}

	_, changes, err := file.Promote(components, "dev", nil)

	require.NoError(t, err)
	require.Len(t, changes, 1)
	assert.Equal(t, "1.0", changes[0].To)
	assert.True(t, changes[0].Downgrade, "an undated release cannot be told to be older, and the pin is not given up silently")
}
