package firmirror

import (
	"context"
	"encoding/xml"
	"os"
	"path/filepath"
	"testing"

	"github.com/klauspost/compress/zstd"
	"github.com/premday/firmirror/pkg/lvfs"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const baseURLCabName = "0cc4c49f6e9701519462f3aafc7329ed066216c0beda7b2b131b0f4ed00fbd98-BCM237.1.148.0_BCM957412N4120HC.fwpkg.cab"

func baseURLTestComponent() lvfs.Component {
	return lvfs.Component{
		Type:            "firmware",
		ID:              "com.test.firmware1",
		Name:            "Test Firmware 1",
		MetadataLicense: "proprietary",
		Releases: []lvfs.Release{{
			Version:   "1.0.0",
			Checksums: []lvfs.Checksum{{Filename: "firmware1.bin", Type: "sha256", Value: "abc123"}},
			Artifacts: []lvfs.Artifact{{Type: "binary", Location: baseURLCabName}},
		}},
	}
}

func newBaseURLSyncer(t *testing.T, dir, baseURL string) *FirmirrorSyncer {
	t.Helper()
	storage, err := NewLocalStorage(dir)
	require.NoError(t, err)
	syncer, err := NewFirmirrorSyncer(FirmirrorConfig{
		CacheDir:        filepath.Join(dir, "cache"),
		FirmwareBaseURL: baseURL,
	}, storage)
	require.NoError(t, err)
	return syncer
}

func readSavedMetadata(t *testing.T, dir string) lvfs.Components {
	t.Helper()
	file, err := os.Open(filepath.Join(dir, "metadata.xml.zst"))
	require.NoError(t, err)
	defer file.Close()
	zstReader, err := zstd.NewReader(file)
	require.NoError(t, err)
	defer zstReader.Close()
	var components lvfs.Components
	require.NoError(t, xml.NewDecoder(zstReader).Decode(&components))
	return components
}

func TestSaveMetadataFirmwareBaseURL(t *testing.T) {
	// fwupd feeds a release location to curl before it re-bases it on the
	// remote's FirmwareBaseURI, so a bare file name is rejected with
	// "failed to parse url" whenever that option is set.
	t.Run("WritesAbsoluteLocations", func(t *testing.T) {
		dir := t.TempDir()
		syncer := newBaseURLSyncer(t, dir, "https://mirror.example.com/fwupd/")
		syncer.newComponents = []lvfs.Component{baseURLTestComponent()}
		require.NoError(t, syncer.SaveMetadata(context.Background()))

		release := readSavedMetadata(t, dir).Component[0].Releases[0]
		want := "https://mirror.example.com/fwupd/" + baseURLCabName
		assert.Equal(t, want, release.Location)
		assert.Equal(t, want, release.Artifacts[0].Location)
	})

	t.Run("LeavesLocationsBareWithoutBaseURL", func(t *testing.T) {
		dir := t.TempDir()
		syncer := newBaseURLSyncer(t, dir, "")
		syncer.newComponents = []lvfs.Component{baseURLTestComponent()}
		require.NoError(t, syncer.SaveMetadata(context.Background()))

		release := readSavedMetadata(t, dir).Component[0].Releases[0]
		assert.Equal(t, baseURLCabName, release.Location)
		assert.Equal(t, baseURLCabName, release.Artifacts[0].Location)
	})

	t.Run("DoesNotMutateTheComponentsItSaves", func(t *testing.T) {
		dir := t.TempDir()
		syncer := newBaseURLSyncer(t, dir, "https://mirror.example.com/fwupd")
		syncer.newComponents = []lvfs.Component{baseURLTestComponent()}
		require.NoError(t, syncer.SaveMetadata(context.Background()))

		assert.Equal(t, baseURLCabName, syncer.newComponents[0].Releases[0].Artifacts[0].Location)
	})

	t.Run("LoadedMetadataGoesBackToBareLocations", func(t *testing.T) {
		// Cleanup matches locations against stored object names, so a run
		// that reloads absolute locations must not see them as unreferenced.
		dir := t.TempDir()
		syncer := newBaseURLSyncer(t, dir, "https://mirror.example.com/fwupd")
		syncer.newComponents = []lvfs.Component{baseURLTestComponent()}
		require.NoError(t, syncer.SaveMetadata(context.Background()))

		reloaded := newBaseURLSyncer(t, dir, "https://mirror.example.com/fwupd")
		require.NoError(t, reloaded.LoadMetadata(context.Background()))
		release := reloaded.existingMetadata.Component[0].Releases[0]
		assert.Equal(t, baseURLCabName, release.Location)
		assert.Equal(t, baseURLCabName, release.Artifacts[0].Location)
	})

	t.Run("RewritesExistingMetadataWhenTheBaseURLChanges", func(t *testing.T) {
		dir := t.TempDir()
		first := newBaseURLSyncer(t, dir, "")
		first.newComponents = []lvfs.Component{baseURLTestComponent()}
		require.NoError(t, first.SaveMetadata(context.Background()))

		// Nothing new to mirror, but the stored locations are the bare ones
		// fwupd rejects.
		second := newBaseURLSyncer(t, dir, "https://mirror.example.com/fwupd")
		require.NoError(t, second.LoadMetadata(context.Background()))
		require.NoError(t, second.SaveMetadata(context.Background()))

		release := readSavedMetadata(t, dir).Component[0].Releases[0]
		assert.Equal(t, "https://mirror.example.com/fwupd/"+baseURLCabName, release.Location)
	})
}
