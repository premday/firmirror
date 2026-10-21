package bkc

import (
	"bytes"
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

	assert.Equal(t, []Overlap{{Ring: "dev", GUID: "shared", ComponentID: "b", Version: "2.0", OtherID: "a", OtherVersion: "1.0"}}, report.Overlaps,
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
	assert.Zero(t, report.OverlapCount)
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

	assert.Equal(t, []Overlap{{Ring: "dev", GUID: "shared", ComponentID: "b", Version: "1.0", OtherID: "a", OtherVersion: "1.0"}}, report.Overlaps)
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
		report.Overlaps = append(report.Overlaps, Overlap{Ring: "dev", GUID: "g"})
		report.OverlapCount++
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
