package bkc

import (
	"bytes"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"github.com/premday/firmirror/pkg/lvfs"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func release(version, date string, tags ...string) lvfs.Release {
	return lvfs.Release{Version: version, Date: date, Tags: tags}
}

func component(id string, releases ...lvfs.Release) lvfs.Component {
	return lvfs.Component{ID: id, Name: "Name of " + id, Releases: releases}
}

// platform builds a component whose ID is shared with others, told apart by guid.
func platform(id, guid string, releases ...lvfs.Release) lvfs.Component {
	c := component(id, releases...)
	c.Provides = []lvfs.Firmware{{Type: "flashed", Text: guid}}
	return c
}

func testFile(t *testing.T, entries ...Entry) *File {
	t.Helper()
	file := New(DefaultRings)
	file.Components = entries
	require.NoError(t, file.Validate())
	return file
}

func tagsByVersion(c lvfs.Component) map[string][]string {
	tags := make(map[string][]string)
	for _, r := range c.Releases {
		tags[r.Version] = r.Tags
	}
	return tags
}

func TestApplyTagsTheReleaseEachRingServes(t *testing.T) {
	file := testFile(t, Entry{ID: "a", Versions: map[string]string{"dev": "3.0", "beta": "2.0", "stable": "1.0"}})
	components := []lvfs.Component{component("a", release("1.0", "2026-01-01"), release("2.0", "2026-02-01"), release("3.0", "2026-03-01"))}

	report := file.Apply(components)

	assert.Equal(t, map[string][]string{
		"1.0": {"firmirror-stable"},
		"2.0": {"firmirror-beta"},
		"3.0": {"firmirror-dev"},
	}, tagsByVersion(components[0]))
	assert.Equal(t, map[string]int{"dev": 1, "beta": 1, "stable": 1}, report.Tagged)
}

func TestApplyGivesARingSharingARelease(t *testing.T) {
	file := testFile(t, Entry{ID: "a", Versions: map[string]string{"dev": "2.0", "beta": "2.0"}})
	components := []lvfs.Component{component("a", release("1.0", "2026-01-01"), release("2.0", "2026-02-01"))}

	file.Apply(components)

	assert.Equal(t, []string{"firmirror-beta", "firmirror-dev"}, components[0].Releases[1].Tags)
}

func TestApplyKeepsTheTagsAReleaseHas(t *testing.T) {
	file := testFile(t, Entry{ID: "a", Versions: map[string]string{"dev": "2.0"}})
	components := []lvfs.Component{component("a",
		release("1.0", "2026-01-01", "other-tag"),
		release("2.0", "2026-02-01", "firmirror-eu-dev"),
	)}

	file.Apply(components)

	assert.Equal(t, []string{"other-tag"}, components[0].Releases[0].Tags)
	assert.Equal(t, []string{"firmirror-dev", "firmirror-eu-dev"}, components[0].Releases[1].Tags, "another prefix's tags are not this file's to remove")
}

func TestApplyServesNothingForAComponentTheFileDoesNotName(t *testing.T) {
	file := testFile(t, Entry{ID: "a", Versions: map[string]string{"dev": "1.0"}})
	components := []lvfs.Component{
		component("a", release("1.0", "2026-01-01")),
		component("b", release("1.0", "2026-01-01")),
	}

	report := file.Apply(components)

	assert.Empty(t, components[1].Releases[0].Tags, "what the file does not name is not served")
	assert.Equal(t, 1, report.Unmanaged)
}

func TestApplyReportsWhatItCannotServe(t *testing.T) {
	file := testFile(t,
		Entry{ID: "a", Versions: map[string]string{"dev": "9.9", "beta": "1.0"}},
		Entry{ID: "gone", Versions: map[string]string{"dev": "1.0"}},
	)
	components := []lvfs.Component{component("a", release("1.0", "2026-01-01"))}

	report := file.Apply(components)

	assert.Equal(t, []Gap{{Ring: "dev", ComponentID: "a", Name: "Name of a", Version: "9.9"}}, report.Dangling,
		"a ring serves nothing for a version the mirror no longer holds")
	assert.Equal(t, []string{"gone"}, report.Unknown)
	assert.Equal(t, []string{"firmirror-beta"}, components[0].Releases[0].Tags, "the rest is still served")
}

func TestApplyTellsPlatformsOfOneIDApart(t *testing.T) {
	file := testFile(t,
		Entry{ID: "a", Platform: Platform([]string{"g1"}), Versions: map[string]string{"dev": "1.0"}},
		Entry{ID: "a", Platform: Platform([]string{"g2"}), Versions: map[string]string{"dev": "2.0"}},
	)
	components := []lvfs.Component{
		platform("a", "g1", release("1.0", "2026-01-01"), release("2.0", "2026-02-01")),
		platform("a", "g2", release("2.0", "2026-02-01")),
	}

	file.Apply(components)

	assert.Equal(t, []string{"firmirror-dev"}, components[0].Releases[0].Tags)
	assert.Empty(t, components[0].Releases[1].Tags, "g1 serves 1.0 in dev, not the 2.0 g2 serves")
	assert.Equal(t, []string{"firmirror-dev"}, components[1].Releases[0].Tags)
}

func TestApplyTagsTheNewestOfTwoReleasesOfOneVersion(t *testing.T) {
	file := testFile(t, Entry{ID: "a", Versions: map[string]string{"dev": "1.0"}})
	older, newer := release("1.0", "2026-01-01"), release("1.0", "2026-02-01")
	components := []lvfs.Component{component("a", older, newer)}

	file.Apply(components)

	assert.Empty(t, components[0].Releases[0].Tags)
	assert.Equal(t, []string{"firmirror-dev"}, components[0].Releases[1].Tags, "fwupd never has two releases of one ring to choose between")
}

func TestApplyTagsTheReleasesItIsGiven(t *testing.T) {
	file := testFile(t, Entry{ID: "a", Versions: map[string]string{"dev": "1.0"}})
	owned := []lvfs.Release{release("1.0", "2026-01-01")}
	components := []lvfs.Component{component("a", owned...)}
	components[0].Releases = owned

	file.Apply(components)

	assert.Equal(t, []string{"firmirror-dev"}, owned[0].Tags, "the releases of a document being built are tagged where they are, not copied")
}

func TestUpstream(t *testing.T) {
	file := New(DefaultRings)

	upstream, err := file.Upstream("preview")
	require.NoError(t, err)
	assert.Equal(t, "beta", upstream)

	upstream, err = file.Upstream("dev")
	require.NoError(t, err)
	assert.Empty(t, upstream, "the first ring takes the newest mirrored release")
	_, err = file.Upstream("nowhere")
	assert.ErrorContains(t, err, `unknown ring "nowhere"`)
}

func TestApplyReportsAnEntryWithoutThePlatformTheMirrorHas(t *testing.T) {
	file := testFile(t, Entry{ID: "a", Versions: map[string]string{"dev": "1.0"}})
	components := []lvfs.Component{platform("a", "g1", release("1.0", "2026-01-01"))}

	report := file.Apply(components)

	assert.Empty(t, report.Unknown, "the mirror has the ID, so it is not the component that is missing")
	require.Len(t, report.Mismatched, 1)
	assert.Equal(t, "a", report.Mismatched[0].ID)
	assert.Equal(t, 1, report.Unmanaged)
	assert.Empty(t, components[0].Releases[0].Tags)
}

func TestPlatformOfIsTheDigestOfTheKeysGUIDs(t *testing.T) {
	both := platform("a", "g2")
	both.Provides = append(both.Provides, lvfs.Firmware{Type: "flashed", Text: "g1"}, lvfs.Firmware{Type: "flashed", Text: "g1"})
	assert.Equal(t, Platform([]string{"g1", "g2"}), platformOf(both))
	assert.Empty(t, platformOf(component("a")))
	assert.Equal(t, "a\n"+Platform([]string{"g1"}), keyOf(platform("a", "g1")))
}

func TestApplyReportsTwoComponentsSharingAGUIDServedApart(t *testing.T) {
	file := testFile(t,
		Entry{ID: "a", Platform: Platform([]string{"g1", "shared"}), Versions: map[string]string{"dev": "1.0"}},
		Entry{ID: "b", Platform: Platform([]string{"shared"}), Versions: map[string]string{"dev": "2.0", "beta": "1.0"}},
	)
	a := platform("a", "g1", release("1.0", "2026-01-01"))
	a.Provides = append(a.Provides, lvfs.Firmware{Type: "flashed", Text: "shared"})
	b := platform("b", "shared", release("1.0", "2026-01-01"), release("2.0", "2026-02-01"))

	report := file.Apply([]lvfs.Component{a, b})

	assert.Equal(t, []Overlap{{Rings: []string{"dev"}, ComponentID: "a", Platform: Platform([]string{"g1", "shared"}), Version: "1.0",
		OtherID: "b", OtherPlatform: Platform([]string{"shared"}), OtherVersion: "2.0", Shared: 1, Only: 1}}, report.Overlaps,
		"beta is served only by b, and a ring serving the same version through both is no hazard")
}

func TestApplyDoesNotReportAGUIDSharedByTwoComponentsServedTheSameVersion(t *testing.T) {
	file := testFile(t,
		Entry{ID: "a", Platform: Platform([]string{"g1", "shared"}), Versions: map[string]string{"dev": "1.0"}},
		Entry{ID: "b", Platform: Platform([]string{"shared"}), Versions: map[string]string{"dev": "1.0"}},
	)
	a := platform("a", "g1", release("1.0", "2026-01-01"))
	a.Provides = append(a.Provides, lvfs.Firmware{Type: "flashed", Text: "shared"})
	b := platform("b", "shared", release("1.0", "2026-01-01"))

	report := file.Apply([]lvfs.Component{a, b})

	assert.Empty(t, report.Overlaps)
}

func TestApplyReportsTwoComponentsSharingAGUIDServedTwoPackagesOfOneVersion(t *testing.T) {
	file := testFile(t,
		Entry{ID: "a", Platform: Platform([]string{"g1", "shared"}), Versions: map[string]string{"dev": "1.0"}},
		Entry{ID: "b", Platform: Platform([]string{"shared"}), Versions: map[string]string{"dev": "1.0"}},
	)
	first := release("1.0", "2026-01-01")
	first.Location = "a.cab"
	second := release("1.0", "2026-01-01")
	second.Location = "b.cab"
	a := platform("a", "g1", first)
	a.Provides = append(a.Provides, lvfs.Firmware{Type: "flashed", Text: "shared"})
	b := platform("b", "shared", second)

	report := file.Apply([]lvfs.Component{a, b})

	assert.Equal(t, []Overlap{{Rings: []string{"dev"}, ComponentID: "a", Platform: Platform([]string{"g1", "shared"}), Version: "1.0",
		OtherID: "b", OtherPlatform: Platform([]string{"shared"}), OtherVersion: "1.0", Shared: 1, Only: 1}}, report.Overlaps)
}

func TestApplyReportsAVersionTheMirrorHoldsSeveralTimes(t *testing.T) {
	file := testFile(t, Entry{ID: "a", Versions: map[string]string{"dev": "1.0", "beta": "2.0"}})
	components := []lvfs.Component{component("a", release("1.0", "2026-01-01"), release("1.0", "2026-02-01"), release("2.0", "2026-03-01"))}

	report := file.Apply(components)

	assert.Equal(t, []Gap{{Ring: "dev", ComponentID: "a", Name: "Name of a", Version: "1.0"}}, report.Reissued)
	assert.Empty(t, components[0].Releases[0].Tags)
	assert.Equal(t, []string{"firmirror-dev"}, components[0].Releases[1].Tags, "the newest is the one tagged")
}

func TestLogToSaysOnlyTheFirstOfEachKind(t *testing.T) {
	report := &Report{}
	for i := 0; i < 3*maxLogged; i++ {
		report.Overlaps = append(report.Overlaps, Overlap{Rings: []string{"dev"}, Shared: 1, Only: 1, OtherOnly: 1})
		report.Unknown = append(report.Unknown, "id")
	}
	var out bytes.Buffer

	report.LogTo(slog.New(slog.NewTextHandler(&out, nil)))

	assert.Equal(t, 2*(maxLogged+1), strings.Count(out.String(), "\n"), "the rest is counted, not repeated")
	assert.Equal(t, 2, strings.Count(out.String(), "more=20"))
}

func TestValidateLeavesTheFileAlone(t *testing.T) {
	file := &File{Version: fileVersion, Rings: []string{"dev"}}

	require.NoError(t, file.Validate())

	assert.Empty(t, file.TagPrefix, "validating is not defaulting")
	assert.Equal(t, DefaultTagPrefix+"-dev", file.Tag("dev"), "a file that names no prefix has the default one all the same")
}

func TestAnEmptyGUIDIsNoGUIDTwoComponentsShare(t *testing.T) {
	file := testFile(t,
		Entry{ID: "a", Platform: Platform([]string{"g1"}), Versions: map[string]string{"dev": "1.0"}},
		Entry{ID: "b", Platform: Platform([]string{"g2"}), Versions: map[string]string{"dev": "2.0"}},
	)
	a := platform("a", "g1", release("1.0", "2026-01-01"))
	a.Provides = append(a.Provides, lvfs.Firmware{Type: "flashed", Text: ""})
	b := platform("b", "g2", release("2.0", "2026-01-01"), release("1.0", "2025-01-01"))
	b.Provides = append(b.Provides, lvfs.Firmware{Type: "flashed", Text: ""})

	report := file.Apply([]lvfs.Component{a, b})

	assert.Empty(t, report.Overlaps)
}

// guided gives a component the GUIDs, which platform gives it one of.
func guided(id string, guids []string, releases ...lvfs.Release) lvfs.Component {
	c := component(id, releases...)
	for _, guid := range guids {
		c.Provides = append(c.Provides, lvfs.Firmware{Type: "flashed", Text: guid})
	}
	return c
}

// entryOf is the entry of a component, served the versions given by ring.
func entryOf(c lvfs.Component, versions map[string]string) Entry {
	return Entry{ID: c.ID, Platform: platformOf(c), Versions: versions}
}

// logOf is what a report logs.
func logOf(report *Report) string {
	var out bytes.Buffer
	report.LogTo(slog.New(slog.NewTextHandler(&out, nil)))
	return out.String()
}

func TestApplyReportsAPairOnceWhateverGUIDsItShares(t *testing.T) {
	a := guided("a", []string{"g1", "g2", "g3"}, release("1.0", "2026-01-01"))
	b := guided("b", []string{"g1", "g2", "g4", "g5"}, release("2.0", "2026-01-01"))
	file := testFile(t,
		entryOf(a, map[string]string{"dev": "1.0"}),
		entryOf(b, map[string]string{"dev": "2.0"}),
	)

	report := file.Apply([]lvfs.Component{a, b})

	assert.Equal(t, []Overlap{{
		Rings: []string{"dev"}, ComponentID: "a", Platform: platformOf(a), Version: "1.0", OtherID: "b", OtherPlatform: platformOf(b), OtherVersion: "2.0",
		Shared: 2, Only: 1, OtherOnly: 2, OnlyGUIDs: []string{"g3"}, OtherOnlyGUIDs: []string{"g4", "g5"},
	}}, report.Overlaps, "the GUIDs only one has are named, as they tell which devices depend on which")
	assert.False(t, report.Overlaps[0].Covered(), "each has GUIDs of its own")
}

func TestApplyReportsTheRingsServingAPairTogether(t *testing.T) {
	a := guided("a", []string{"g1"}, release("1.0", "2026-01-01"))
	b := guided("b", []string{"g1"}, release("2.0", "2026-01-01"), release("3.0", "2026-02-01"))
	file := testFile(t,
		entryOf(a, map[string]string{"dev": "1.0", "beta": "1.0", "preview": "1.0"}),
		entryOf(b, map[string]string{"dev": "2.0", "beta": "2.0", "preview": "3.0"}),
	)

	report := file.Apply([]lvfs.Component{a, b})

	assert.Equal(t, []Overlap{
		{Rings: []string{"dev", "beta"}, ComponentID: "a", Platform: platformOf(a), Version: "1.0", OtherID: "b", OtherPlatform: platformOf(b), OtherVersion: "2.0", Shared: 1},
		{Rings: []string{"preview"}, ComponentID: "a", Platform: platformOf(a), Version: "1.0", OtherID: "b", OtherPlatform: platformOf(b), OtherVersion: "3.0", Shared: 1},
	}, report.Overlaps, "the rings that serve the same two releases are one entry")
	assert.True(t, report.Overlaps[0].Covered(), "the same GUIDs on both sides")
}

func TestSplitOverlapsSeparatesThePairsThatEachHaveGUIDsOfTheirOwn(t *testing.T) {
	partial, covered := splitOverlaps([]Overlap{
		{ComponentID: "a", Only: 1},
		{ComponentID: "b", Only: 1, OtherOnly: 1},
		{ComponentID: "c", OtherOnly: 2},
		{ComponentID: "d", Only: 2, OtherOnly: 1},
	})

	assert.Equal(t, []string{"b", "d"}, []string{partial[0].ComponentID, partial[1].ComponentID}, "those that need a decision, in the order they came")
	assert.Equal(t, []string{"a", "c"}, []string{covered[0].ComponentID, covered[1].ComponentID})
}

func TestApplyTellsThePlatformsOfOneIDApart(t *testing.T) {
	// Dell publishes one ID for several platforms, which share GUIDs with others.
	first := guided("a", []string{"g1", "shared"}, release("1.0", "2026-01-01"))
	second := guided("a", []string{"g2", "shared"}, release("2.0", "2026-01-01"))
	other := guided("b", []string{"g3", "shared"}, release("3.0", "2026-01-01"))
	components := []lvfs.Component{first, second, other}
	var entries []Entry
	for _, c := range components {
		entries = append(entries, entryOf(c, map[string]string{"dev": c.Releases[0].Version}))
	}

	report := testFile(t, entries...).Apply(components)

	require.Len(t, report.Overlaps, 3)
	pairs := make(map[[2]string]bool)
	for _, overlap := range report.Overlaps {
		pairs[[2]string{overlap.Platform, overlap.OtherPlatform}] = true
	}
	assert.Equal(t, map[[2]string]bool{
		{platformOf(first), platformOf(second)}: true,
		{platformOf(first), platformOf(other)}:  true,
		{platformOf(second), platformOf(other)}: true,
	}, pairs, "two pairs of the same IDs differ only by their platforms")
}

func TestApplyNamesAtMostSomeOfTheGUIDsOnlyOneHas(t *testing.T) {
	var many []string
	for i := 0; i < 3*maxLogged; i++ {
		many = append(many, fmt.Sprintf("only-%02d", i))
	}
	a := guided("a", append([]string{"shared"}, many...), release("1.0", "2026-01-01"))
	b := guided("b", []string{"shared", "elsewhere"}, release("2.0", "2026-01-01"))

	report := testFile(t,
		entryOf(a, map[string]string{"dev": "1.0"}),
		entryOf(b, map[string]string{"dev": "2.0"}),
	).Apply([]lvfs.Component{a, b})

	require.Len(t, report.Overlaps, 1)
	assert.Equal(t, 3*maxLogged, report.Overlaps[0].Only, "the count is all of them")
	assert.Equal(t, many[:maxLogged], report.Overlaps[0].OnlyGUIDs, "the GUIDs named are the first")
	assert.Equal(t, []string{"elsewhere"}, report.Overlaps[0].OtherOnlyGUIDs)
}

func TestApplyNamesNoGUIDsOfAPairOneOfWhichCoversTheOther(t *testing.T) {
	a := guided("a", []string{"g1", "g2"}, release("1.0", "2026-01-01"))
	b := guided("b", []string{"g1"}, release("2.0", "2026-01-01"))

	report := testFile(t,
		entryOf(a, map[string]string{"dev": "1.0"}),
		entryOf(b, map[string]string{"dev": "2.0"}),
	).Apply([]lvfs.Component{a, b})

	require.Len(t, report.Overlaps, 1)
	assert.Empty(t, report.Overlaps[0].OnlyGUIDs)
	assert.Empty(t, report.Overlaps[0].OtherOnlyGUIDs)
}

func TestApplyKeepsOnlySomePairsWhenThereAreMoreThanItCanHold(t *testing.T) {
	var components []lvfs.Component
	var entries []Entry
	for i := 0; i < 6; i++ {
		c := guided(fmt.Sprintf("c%d", i), []string{"shared"}, release(fmt.Sprintf("%d.0", i), "2026-01-01"))
		components = append(components, c)
		entries = append(entries, entryOf(c, map[string]string{"dev": c.Releases[0].Version}))
	}
	file := testFile(t, entries...)
	bounds := overlapBounds{pairs: 4}

	report := file.apply(components, bounds)
	again := file.apply(components, bounds)

	assert.Len(t, report.Overlaps, 4, "6 components make 15 pairs")
	assert.True(t, report.OverlapsTruncated)
	assert.Equal(t, report.Overlaps, again.Overlaps, "the ones kept are always the same")
}

func TestApplyStopsLookingForPairsPastItsBudget(t *testing.T) {
	var components []lvfs.Component
	var entries []Entry
	for i := 0; i < 6; i++ {
		c := guided(fmt.Sprintf("c%d", i), []string{"shared"}, release(fmt.Sprintf("%d.0", i), "2026-01-01"))
		components = append(components, c)
		entries = append(entries, entryOf(c, map[string]string{"dev": c.Releases[0].Version}))
	}

	report := testFile(t, entries...).apply(components, overlapBounds{visits: 5})

	assert.True(t, report.OverlapsTruncated, "6 components share a GUID, which is 15 pairs to look at")
	assert.Len(t, report.Overlaps, 5, "the pairs found within the budget are kept: the first component with each of the others")
}

func TestApplyNamesTheGUIDsOnlyForThePairsThatAreLogged(t *testing.T) {
	var components []lvfs.Component
	var entries []Entry
	for i := 0; i < maxLogged+2; i++ {
		a := guided(fmt.Sprintf("a%02d", i), []string{fmt.Sprintf("shared%02d", i), fmt.Sprintf("a-own%02d", i)}, release("1.0", "2026-01-01"))
		b := guided(fmt.Sprintf("b%02d", i), []string{fmt.Sprintf("shared%02d", i), fmt.Sprintf("b-own%02d", i)}, release("2.0", "2026-01-01"))
		components = append(components, a, b)
		entries = append(entries, entryOf(a, map[string]string{"dev": "1.0"}), entryOf(b, map[string]string{"dev": "2.0"}))
	}

	report := testFile(t, entries...).Apply(components)

	require.Len(t, report.Overlaps, maxLogged+2)
	assert.NotEmpty(t, report.Overlaps[maxLogged-1].OnlyGUIDs, "the last pair that is logged")
	assert.Empty(t, report.Overlaps[maxLogged].OnlyGUIDs, "the others are only counted")
	assert.Equal(t, 1, report.Overlaps[maxLogged].Only)
}

func TestApplyCountsThePairsItKeptFromTheirGUIDsWhateverItLookedAt(t *testing.T) {
	guids := []string{"g1", "g2", "g3", "g4"}
	a := guided("a", guids, release("1.0", "2026-01-01"))
	b := guided("b", guids, release("2.0", "2026-01-01"))

	file := testFile(t, entryOf(a, map[string]string{"dev": "1.0"}), entryOf(b, map[string]string{"dev": "2.0"}))

	report := file.apply([]lvfs.Component{a, b}, overlapBounds{visits: 3})

	require.True(t, report.OverlapsTruncated, "the budget ran out before every GUID was looked at")
	require.Len(t, report.Overlaps, 1)
	assert.Equal(t, 4, report.Overlaps[0].Shared)
	assert.True(t, report.Overlaps[0].Covered(), "the same GUIDs on both sides is not a pair that each have GUIDs of their own")
}

func TestApplyDoesNotLookAtComponentsServedTheSameRelease(t *testing.T) {
	var components []lvfs.Component
	var entries []Entry
	for i := 0; i < 20; i++ {
		c := guided(fmt.Sprintf("c%02d", i), []string{"shared"}, release("1.0", "2026-01-01"))
		components = append(components, c)
		entries = append(entries, entryOf(c, map[string]string{"dev": "1.0"}))
	}

	report := testFile(t, entries...).apply(components, overlapBounds{visits: 1})

	assert.Empty(t, report.Overlaps, "the same release through every one of them")
	assert.False(t, report.OverlapsTruncated, "190 pairs of components, none of which is a visit")
}

func TestApplyIsNotTruncatedWithinTheBound(t *testing.T) {
	a := guided("a", []string{"g"}, release("1.0", "2026-01-01"))
	b := guided("b", []string{"g"}, release("2.0", "2026-01-01"))

	report := testFile(t,
		entryOf(a, map[string]string{"dev": "1.0"}),
		entryOf(b, map[string]string{"dev": "2.0"}),
	).Apply([]lvfs.Component{a, b})

	assert.False(t, report.OverlapsTruncated)
}

func TestLogToSaysWhenThePairsAreNotAll(t *testing.T) {
	assert.Contains(t, logOf(&Report{OverlapsTruncated: true}), "looked at or kept")
}

func TestLogToSaysWhichPairsEachHaveGUIDsOfTheirOwn(t *testing.T) {
	report := &Report{Overlaps: []Overlap{
		{Rings: []string{"dev"}, ComponentID: "a", Platform: "p1", OtherID: "b", OtherPlatform: "p2", Shared: 2, Only: 1, OtherOnly: 2,
			OnlyGUIDs: []string{"g1"}, OtherOnlyGUIDs: []string{"g2"}},
		{Rings: []string{"dev"}, ComponentID: "c", OtherID: "d", Shared: 3, Only: 0, OtherOnly: 2},
	}}

	lines := strings.Split(strings.TrimSpace(logOf(report)), "\n")
	require.Len(t, lines, 2)
	assert.Contains(t, lines[0], "each has GUIDs of its own")
	assert.Contains(t, lines[0], "id=a")
	assert.Contains(t, lines[0], "platform=p1")
	assert.Contains(t, lines[0], "other_platform=p2")
	assert.Contains(t, lines[0], "shared=2 only=1 other_only=2")
	assert.Contains(t, lines[0], "only_guids=g1 ")
	assert.Contains(t, lines[0], "other_only_guids=g2,...", "it says when the GUIDs named are not all")
	assert.Contains(t, lines[1], "provides every GUID of another")
	assert.Contains(t, lines[1], "id=c")
}

func TestApplyLeavesAHeldComponentOutOfEveryRing(t *testing.T) {
	file := testFile(t,
		Entry{ID: "a", Hold: true, Versions: map[string]string{"dev": "1.0", "beta": "9.9"}},
		Entry{ID: "b", Versions: map[string]string{"dev": "1.0"}},
	)
	components := []lvfs.Component{component("a", release("1.0", "2026-01-01")), component("b", release("1.0", "2026-01-01"))}

	report := file.Apply(components)

	assert.Empty(t, components[0].Releases[0].Tags)
	assert.Equal(t, []string{"firmirror-dev"}, components[1].Releases[0].Tags)
	assert.Equal(t, 1, report.Held)
	assert.Equal(t, map[string]int{"dev": 1}, report.Tagged)
	assert.Empty(t, report.Dangling, "a version it is not served is no gap")
	assert.Empty(t, report.Unknown)
	assert.Zero(t, report.Unmanaged, "it has an entry")
}

func TestApplyDoesNotReportAHeldComponentAsOverlapping(t *testing.T) {
	older := guided("a", []string{"g1"}, release("1.0", "2026-01-01"))
	newer := guided("b", []string{"g1"}, release("2.0", "2026-01-01"))
	file := testFile(t,
		Entry{ID: "a", Platform: platformOf(older), Hold: true, Versions: map[string]string{"dev": "1.0"}},
		entryOf(newer, map[string]string{"dev": "2.0"}),
	)

	report := file.Apply([]lvfs.Component{older, newer})

	assert.Empty(t, report.Overlaps)
}

func TestLogToSaysHowManyComponentsAreHeld(t *testing.T) {
	out := logOf(&Report{Held: 3})

	assert.Contains(t, out, "held out of every ring")
	assert.Contains(t, out, "components=3")
}

func TestApplyCountsAHeldEntryWithNoPlatformAsHoldingTheComponentsOfItsID(t *testing.T) {
	file := testFile(t,
		Entry{ID: "a", Hold: true, Versions: map[string]string{"dev": "1.0"}},
		Entry{ID: "c", Versions: map[string]string{"dev": "1.0"}},
	)
	components := []lvfs.Component{
		platform("a", "g1", release("1.0", "2026-01-01")),
		platform("a", "g2", release("1.0", "2026-01-01")),
		platform("b", "g3", release("1.0", "2026-01-01")),
	}

	report := file.Apply(components)

	assert.Equal(t, 2, report.Held, "both platforms of a")
	assert.Equal(t, 1, report.Unmanaged, "b has no entry, and no held one")
	assert.Empty(t, report.Mismatched, "the held entry stands for the platforms it names none of")
	assert.Equal(t, []string{"c"}, report.Unknown, "c is not mirrored")
	assert.Empty(t, components[0].Releases[0].Tags)
	assert.Empty(t, components[1].Releases[0].Tags)
}

func TestApplyLeavesTheNewPlatformOfAHeldPackageUnmanagedWhileTheOldOneIsStillMirrored(t *testing.T) {
	held := platform("a", "g1", release("1.0", "2026-01-01"))
	// The vendor added a GUID to the package: the mirror has it under a new platform, and the held one too.
	changed := guided("a", []string{"g1", "g2"}, release("1.0", "2026-01-01"))
	file := testFile(t, Entry{ID: "a", Platform: platformOf(held), Hold: true, Versions: map[string]string{"dev": "1.0"}})

	report := file.Apply([]lvfs.Component{held, changed})

	assert.Equal(t, 1, report.Held, "the held entry still holds its component")
	assert.Equal(t, 1, report.Unmanaged, "the new platform has no entry: it is served once `promote` makes one, and warns of it")
	assert.Empty(t, report.Mismatched)
	assert.Empty(t, changed.Releases[0].Tags)
}

func TestApplyReportsAHeldEntryWhosePlatformIsNoLongerMirrored(t *testing.T) {
	held := platform("a", "g1", release("1.0", "2026-01-01"))
	// As when the stored document was discarded: the mirror has the package under its new platform only.
	changed := guided("a", []string{"g1", "g2"}, release("1.0", "2026-01-01"))
	file := testFile(t, Entry{ID: "a", Platform: platformOf(held), Hold: true, Versions: map[string]string{"dev": "1.0"}})

	report := file.Apply([]lvfs.Component{changed})

	assert.Zero(t, report.Held, "a held entry with a platform holds that component only")
	assert.Equal(t, 1, report.Unmanaged, "the package under its new platform has no entry, which `promote` makes")
	require.Len(t, report.Mismatched, 1, "the held entry is reported as one the mirror lacks, which is how the change shows")
	assert.True(t, report.Mismatched[0].Hold)
	assert.Empty(t, changed.Releases[0].Tags)
}

func TestApplyDoesNotReportAHeldEntryWithNoPlatformWhateverTheComponentsOfItsIDHave(t *testing.T) {
	a := platform("a", "g1", release("1.0", "2026-01-01"))
	file := testFile(t,
		Entry{ID: "a", Hold: true, Versions: map[string]string{"dev": "1.0"}},
		entryOf(a, map[string]string{"dev": "1.0"}),
	)

	report := file.Apply([]lvfs.Component{a})

	assert.Empty(t, report.Mismatched, "it holds its ID whole: the components that come, as well as those that have an entry")
	assert.Zero(t, report.Held, "and none is held now")
}

func TestLogToSaysWhichMismatchedEntryIsAHoldOnNothing(t *testing.T) {
	out := logOf(&Report{Mismatched: []Entry{{ID: "a", Hold: true}, {ID: "b"}}})

	lines := strings.Split(strings.TrimSpace(out), "\n")
	require.Len(t, lines, 2)
	assert.Contains(t, lines[0], "hold=true")
	assert.NotContains(t, lines[1], "hold=")
}
