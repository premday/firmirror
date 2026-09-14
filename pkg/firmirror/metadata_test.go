package firmirror

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/premday/firmirror/pkg/lvfs"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// failingWriteStorage fails the upload of one key, to exercise what a document
// left half-published does to the next run.
type failingWriteStorage struct {
	*LocalStorage
	failKey string
}

func (s *failingWriteStorage) Write(ctx context.Context, key string, data io.Reader) error {
	if key == s.failKey {
		return errors.New("injected write failure")
	}
	return s.LocalStorage.Write(ctx, key, data)
}

// testRelease builds a release the way processEntry does: the vendor firmware
// filename on the checksum, and the content-addressed cabinet as the location.
func testRelease(version, firmware, cab string) lvfs.Release {
	return lvfs.Release{
		Version:   version,
		Checksums: []lvfs.Checksum{{Filename: firmware, Target: "content", Value: "deadbeef"}},
		Location:  cab,
		Artifacts: []lvfs.Artifact{{Type: "binary", Location: cab}},
	}
}

func testComponents(releases ...lvfs.Release) *lvfs.Components {
	components := &lvfs.Components{Origin: "firmirror", SchemaVersion: lvfs.MetadataSchemaVersion}
	for _, release := range releases {
		components.Component = append(components.Component, lvfs.Component{
			Type:     "firmware",
			ID:       "com.test." + release.Version,
			Name:     "Test Firmware " + release.Version,
			Provides: []lvfs.Firmware{{Type: "flashed", Text: "guid-" + release.Version}},
			Releases: []lvfs.Release{release},
		})
	}
	return components
}

func writeTestMetadata(t *testing.T, syncer *FirmirrorSyncer, key string, components *lvfs.Components) {
	t.Helper()
	compressed, err := encodeMetadata(components)
	require.NoError(t, err)
	require.NoError(t, syncer.Storage.Write(context.Background(), key, bytes.NewReader(compressed)))
}

func readTestMetadata(t *testing.T, syncer *FirmirrorSyncer, key string) *lvfs.Components {
	t.Helper()
	components, found, err := syncer.readComponents(context.Background(), key)
	require.NoError(t, err)
	require.True(t, found, "expected %s to exist in storage", key)
	return components
}

func releaseVersions(components *lvfs.Components) []string {
	versions := make([]string, 0)
	for _, component := range components.Component {
		for _, release := range component.Releases {
			versions = append(versions, release.Version)
		}
	}
	return versions
}

func objectKeys(objects []StoredObject) []string {
	keys := make([]string, 0, len(objects))
	for _, object := range objects {
		keys = append(keys, object.Key)
	}
	return keys
}

// captureLogs redirects the default logger for the duration of the test.
func captureLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(previous) })
	return &buf
}

func TestEncodeMetadata(t *testing.T) {
	components := testComponents(
		testRelease("1.0.0", "firmware-v1.bin", "aaa-firmware-v1.bin.cab"),
		testRelease("2.0.0", "firmware-v2.bin", "bbb-firmware-v2.bin.cab"),
	)

	t.Run("IsDeterministic", func(t *testing.T) {
		first, err := encodeMetadata(components)
		require.NoError(t, err)
		second, err := encodeMetadata(components)
		require.NoError(t, err)

		// Feeds are only rewritten when their bytes change, which relies on
		// the same repository always encoding identically.
		assert.Equal(t, first, second)
	})

	t.Run("RoundTripsThroughStorage", func(t *testing.T) {
		syncer, _ := createTestSyncer(t)
		writeTestMetadata(t, syncer, IndexKey, components)

		stored := readTestMetadata(t, syncer, IndexKey)

		assert.Equal(t, []string{"1.0.0", "2.0.0"}, releaseVersions(stored))
		assert.Equal(t, lvfs.MetadataSchemaVersion, stored.SchemaVersion)
		assert.Equal(t, "firmirror", stored.Origin)
	})
}

func TestReadComponents(t *testing.T) {
	t.Run("ReportsAMissingDocument", func(t *testing.T) {
		syncer, _ := createTestSyncer(t)

		components, found, err := syncer.readComponents(context.Background(), IndexKey)

		require.NoError(t, err)
		assert.False(t, found)
		assert.Nil(t, components)
	})

	t.Run("FailsOnADocumentThatIsNotMetadata", func(t *testing.T) {
		syncer, _ := createTestSyncer(t)
		require.NoError(t, syncer.Storage.Write(context.Background(), IndexKey, bytes.NewReader([]byte("not zstd"))))

		_, _, err := syncer.readComponents(context.Background(), IndexKey)

		assert.ErrorContains(t, err, IndexKey)
	})
}

func TestStoredMetadataMatches(t *testing.T) {
	syncer, _ := createTestSyncer(t)
	components := testComponents(testRelease("1.0.0", "firmware-v1.bin", "aaa-firmware-v1.bin.cab"))
	compressed, err := encodeMetadata(components)
	require.NoError(t, err)

	assert.False(t, syncer.storedMetadataMatches(context.Background(), IndexKey, compressed),
		"a document that is not there cannot match")

	writeTestMetadata(t, syncer, IndexKey, components)
	assert.True(t, syncer.storedMetadataMatches(context.Background(), IndexKey, compressed))

	other, err := encodeMetadata(testComponents(testRelease("2.0.0", "firmware-v2.bin", "bbb-firmware-v2.bin.cab")))
	require.NoError(t, err)
	assert.False(t, syncer.storedMetadataMatches(context.Background(), IndexKey, other))
}

func TestCountReleases(t *testing.T) {
	assert.Equal(t, 0, countReleases(nil))
	assert.Equal(t, 2, countReleases(testComponents(
		testRelease("1.0.0", "firmware-v1.bin", "aaa-firmware-v1.bin.cab"),
		testRelease("2.0.0", "firmware-v2.bin", "bbb-firmware-v2.bin.cab"),
	)))
}

func TestWriteSignedMetadata(t *testing.T) {
	ctx := context.Background()
	components := testComponents(testRelease("1.0.0", "firmware-v1.bin", "aaa-firmware-v1.bin.cab"))

	t.Run("WritesTheDocumentAndItsSignature", func(t *testing.T) {
		syncer, tmpDir := createTestSyncer(t)
		compressed, err := encodeMetadata(components)
		require.NoError(t, err)

		require.NoError(t, syncer.writeSignedMetadata(ctx, IndexKey, compressed))

		assert.FileExists(t, filepath.Join(tmpDir, "output", IndexKey))
		assert.FileExists(t, filepath.Join(tmpDir, "output", IndexKey+signatureSuffix))
	})

	t.Run("PublishesNoSignatureForADocumentThatFailedToUpload", func(t *testing.T) {
		syncer, tmpDir := createTestSyncer(t)
		syncer.Storage = &failingWriteStorage{LocalStorage: syncer.Storage.(*LocalStorage), failKey: IndexKey}
		compressed, err := encodeMetadata(components)
		require.NoError(t, err)

		require.ErrorContains(t, syncer.writeSignedMetadata(ctx, IndexKey, compressed), "injected write failure")

		assert.NoFileExists(t, filepath.Join(tmpDir, "output", IndexKey+signatureSuffix),
			"a signature for bytes no client can download fails verification for everyone")
	})

	t.Run("IgnoresAJcatLeftInTheCacheByAKilledRun", func(t *testing.T) {
		syncer, tmpDir := createTestSyncer(t)
		compressed, err := encodeMetadata(components)
		require.NoError(t, err)
		// jcat-tool imports into the JCAT it is handed, so a file a killed
		// run left behind would be published with a checksum for other bytes
		// next to this document's own.
		stale := filepath.Join(syncer.Config.CacheDir, IndexKey+signatureSuffix)
		require.NoError(t, os.WriteFile(stale, []byte("sha256 stale-document\n"), 0644))

		require.NoError(t, syncer.writeSignedMetadata(ctx, IndexKey, compressed))

		published, err := os.ReadFile(filepath.Join(tmpDir, "output", IndexKey+signatureSuffix))
		require.NoError(t, err)
		assert.NotContains(t, string(published), "stale-document")
	})
}
