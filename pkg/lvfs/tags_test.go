package lvfs

import (
	"encoding/xml"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func taggedComponent() Component {
	release := func(version string, tags ...string) Release {
		return Release{Version: version, Date: "2026-01-01", Tags: tags}
	}
	return Component{
		Type:     "firmware",
		ID:       "com.test.firmware",
		Provides: []Firmware{{Type: "flashed", Text: "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"}},
		Releases: []Release{
			release("1.2.4", "firmirror-dev"),
			release("1.2.3"),
			release("1.2.2", "firmirror-preview", "firmirror-stable"),
			release("1.2.3", "firmirror-beta"),
			release("1.2.1"),
		},
	}
}

func TestGroupByTagsDropsTheTagsAComponentWasStoredWith(t *testing.T) {
	stored := &Tags{Tag: []Tag{{Namespace: TagNamespace, Value: "firmirror-dev"}}}
	untagged := Release{Version: "20.0", Date: "2026-02-01"}
	tagged := Release{Version: "10.0", Date: "2026-01-01", Tags: []string{"firmirror-dev"}}

	for name, releases := range map[string][]Release{
		"nothing tagged":       {untagged},
		"some releases tagged": {untagged, tagged},
	} {
		t.Run(name, func(t *testing.T) {
			grouped := GroupByTags([]Component{{ID: "com.test.firmware", Tags: stored, Releases: releases}})

			require.NotEmpty(t, grouped)
			assert.Nil(t, grouped[0].Tags, "a release no ring names is served by no ring")
			assert.Equal(t, "20.0", grouped[0].Releases[0].Version)
		})
	}
}

func TestGroupByTags(t *testing.T) {
	grouped := GroupByTags([]Component{taggedComponent()})

	type piece struct {
		tags     []string
		versions []string
	}
	var got []piece
	for _, component := range grouped {
		var p piece
		if component.Tags != nil {
			for _, tag := range component.Tags.Tag {
				assert.Equal(t, TagNamespace, tag.Namespace)
				p.tags = append(p.tags, tag.Value)
			}
		}
		for _, release := range component.Releases {
			p.versions = append(p.versions, release.Version)
		}
		got = append(got, p)
	}

	assert.Equal(t, []piece{
		{versions: []string{"1.2.3", "1.2.1"}},
		{tags: []string{"firmirror-beta"}, versions: []string{"1.2.3"}},
		{tags: []string{"firmirror-dev"}, versions: []string{"1.2.4"}},
		{tags: []string{"firmirror-preview", "firmirror-stable"}, versions: []string{"1.2.2"}},
	}, got, "a tag set is shared by its releases, and a tag is never inherited by an untagged release of the same version")

	for _, component := range grouped {
		assert.Equal(t, "com.test.firmware", component.ID)
		assert.Equal(t, "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee", component.Provides[0].Text)
	}
}

func TestTagsAreOmittedWhenThereAreNone(t *testing.T) {
	component := taggedComponent()
	component.Releases = []Release{{Version: "1.0.0"}}

	out, err := xml.Marshal(&Components{Component: GroupByTags([]Component{component})})
	require.NoError(t, err)
	assert.NotContains(t, string(out), "<tags", "an untagged component must publish exactly as before")
}

func TestReleaseTags(t *testing.T) {
	var release Release
	release.AddTag("b")
	release.AddTag("a")
	release.AddTag("b")
	assert.Equal(t, []string{"a", "b"}, release.Tags)
	release.AddTag("keep")
	assert.Equal(t, []string{"a", "b", "keep"}, release.Tags)
}

func TestGroupByTagsDoesNotKeepTheTagsOfAComponentWithNoRelease(t *testing.T) {
	component := taggedComponent()
	component.Releases = nil
	component.Tags = &Tags{Tag: []Tag{{Namespace: TagNamespace, Value: "stale"}}}

	grouped := GroupByTags([]Component{component})

	require.Len(t, grouped, 1)
	assert.Nil(t, grouped[0].Tags)
}
