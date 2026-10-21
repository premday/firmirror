package firmirror

import (
	"context"
	"encoding/xml"
	"os"
	"path/filepath"
	"testing"

	"github.com/klauspost/compress/zstd"
	"github.com/premday/firmirror/pkg/bkc"
	"github.com/premday/firmirror/pkg/lvfs"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const versionedGUID = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"

// definitions is what the rings serve of the component versioned makes: one
// version per ring, in ring order from dev, as many as given.
func definitions(versions ...string) *bkc.File {
	file := bkc.New([]string{"dev", "beta", "stable"})
	entry := bkc.Entry{ID: "com.test.firmware1", Platform: bkc.Platform([]string{versionedGUID}), Versions: map[string]string{}}
	for i, version := range versions {
		entry.Versions[file.Rings[i]] = version
	}
	file.Components = []bkc.Entry{entry}
	if err := file.Validate(); err != nil {
		panic(err)
	}
	return file
}

// versioned is a component with one release at the given version, shipped as cab.
func versioned(version, cab string) lvfs.Component {
	component := baseURLTestComponent()
	component.Provides = []lvfs.Firmware{{Type: "flashed", Text: versionedGUID}}
	component.Releases[0].Version = version
	component.Releases[0].Date = "2026-01-" + version[:2]
	component.Releases[0].Checksums = []lvfs.Checksum{{Filename: "firmware-" + version + ".bin", Type: "sha256", Value: version}}
	component.Releases[0].Artifacts = []lvfs.Artifact{{Type: "binary", Location: cab}}
	return component
}

func newRingsSyncer(t *testing.T, dir string, file *bkc.File) *FirmirrorSyncer {
	t.Helper()
	syncer := newBaseURLSyncer(t, dir, "")
	syncer.Config.BKC = file
	return syncer
}

// publishedTags lists, per version, the tags the stored document serves it with,
// reading them from the components the way fwupd does.
func publishedTags(t *testing.T, dir string) map[string][]string {
	t.Helper()
	published := readSavedMetadata(t, dir)
	tags := make(map[string][]string)
	for _, component := range published.Component {
		var componentTags []string
		if component.Tags != nil {
			for _, tag := range component.Tags.Tag {
				assert.Equal(t, "lvfs", tag.Namespace)
				componentTags = append(componentTags, tag.Value)
			}
		}
		for _, release := range component.Releases {
			assert.Empty(t, release.Tags, "tags are published on the component, never on the release")
			tags[release.Version] = componentTags
		}
	}
	return tags
}

// refresh loads what dir holds and saves it with the components mirrored.
func refresh(t *testing.T, dir string, file *bkc.File, mirrored ...lvfs.Component) {
	t.Helper()
	syncer := newRingsSyncer(t, dir, file)
	require.NoError(t, syncer.LoadMetadata(context.Background()))
	syncer.newComponents = mirrored
	require.NoError(t, syncer.SaveMetadata(context.Background()))
}

func TestSaveMetadataTagsWhatTheDefinitionsSay(t *testing.T) {
	dir := t.TempDir()
	file := definitions("20.0", "10.0")
	refresh(t, dir, file, versioned("10.0", "a.cab"))
	refresh(t, dir, file, versioned("20.0", "b.cab"))

	assert.Equal(t, map[string][]string{
		"10.0": {"firmirror-beta"},
		"20.0": {"firmirror-dev"},
	}, publishedTags(t, dir), "newer firmware is served by no ring until the definitions name it")
}

func TestSaveMetadataPublishesNoRingWithoutDefinitions(t *testing.T) {
	dir := t.TempDir()
	refresh(t, dir, definitions("10.0"), versioned("10.0", "a.cab"))
	require.Equal(t, []string{"firmirror-dev"}, publishedTags(t, dir)["10.0"])

	refresh(t, dir, nil)

	assert.Empty(t, publishedTags(t, dir)["10.0"], "the tags are an output of the definitions, not something a document keeps")
}

func TestNewDefinitionsArePublishedEvenWithNothingNewToMirror(t *testing.T) {
	dir := t.TempDir()
	refresh(t, dir, definitions("10.0"), versioned("10.0", "a.cab"))

	refresh(t, dir, definitions("10.0", "10.0"))

	assert.Equal(t, []string{"firmirror-beta", "firmirror-dev"}, publishedTags(t, dir)["10.0"])
}

func TestUnchangedDefinitionsLeaveTheDocumentAlone(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	refresh(t, dir, definitions("10.0"), versioned("10.0", "a.cab"))

	syncer := newRingsSyncer(t, dir, definitions("10.0"))
	recorder := &writeRecorder{Storage: syncer.Storage}
	syncer.Storage = recorder
	require.NoError(t, syncer.LoadMetadata(ctx))
	require.NoError(t, syncer.SaveMetadata(ctx))

	assert.Empty(t, recorder.keys)
}

func TestARingServesTheReleaseOfThePlatformThatHasIt(t *testing.T) {
	dir := t.TempDir()
	other := versioned("10.0", "other.cab")
	other.Provides[0].Text = "ffffffff-bbbb-cccc-dddd-eeeeeeeeeeee"

	refresh(t, dir, definitions("10.0"), versioned("10.0", "a.cab"), other)

	var tagged int
	for _, component := range readSavedMetadata(t, dir).Component {
		if component.Tags != nil {
			tagged++
			assert.Equal(t, versionedGUID, component.Provides[0].Text, "only the platform the definitions name is served")
		}
	}
	assert.Equal(t, 1, tagged)
}

func TestPublishedDocumentIsWhatFwupdReads(t *testing.T) {
	dir := t.TempDir()
	file := definitions("20.0", "10.0", "10.0")
	refresh(t, dir, file, versioned("10.0", "a.cab"))
	refresh(t, dir, file, versioned("20.0", "b.cab"))

	reader, err := os.Open(filepath.Join(dir, "metadata.xml.zst"))
	require.NoError(t, err)
	defer reader.Close()
	decoder, err := zstd.NewReader(reader)
	require.NoError(t, err)
	defer decoder.Close()

	var raw struct {
		Components []struct {
			ID   string `xml:"id"`
			GUID string `xml:"provides>firmware"`
			Tags []struct {
				Namespace string `xml:"namespace,attr"`
				Value     string `xml:",chardata"`
			} `xml:"tags>tag"`
			Releases []struct {
				Version string `xml:"version,attr"`
			} `xml:"releases>release"`
		} `xml:"component"`
	}
	require.NoError(t, xml.NewDecoder(decoder).Decode(&raw))

	byVersion := make(map[string][]string)
	for _, component := range raw.Components {
		assert.Equal(t, "com.test.firmware1", component.ID)
		assert.Equal(t, versionedGUID, component.GUID, "every piece must still match the device")
		for _, release := range component.Releases {
			for _, tag := range component.Tags {
				byVersion[release.Version] = append(byVersion[release.Version], tag.Namespace+":"+tag.Value)
			}
		}
	}
	assert.Equal(t, []string{"lvfs:firmirror-dev"}, byVersion["20.0"])
	assert.Equal(t, []string{"lvfs:firmirror-beta", "lvfs:firmirror-stable"}, byVersion["10.0"])
}

func TestNewFirmirrorSyncerRejectsDefinitionsThatWereNotValidated(t *testing.T) {
	storage, err := NewLocalStorage(t.TempDir())
	require.NoError(t, err)

	_, err = NewFirmirrorSyncer(FirmirrorConfig{CacheDir: t.TempDir(), BKC: &bkc.File{}}, storage)

	assert.ErrorContains(t, err, "invalid rollout rings")
}
