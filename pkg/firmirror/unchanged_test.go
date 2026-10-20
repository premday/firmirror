package firmirror

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/premday/firmirror/pkg/lvfs"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// writeRecorder records the keys written through it.
type writeRecorder struct {
	Storage
	keys []string
}

func (r *writeRecorder) Write(ctx context.Context, key string, data io.Reader) error {
	r.keys = append(r.keys, key)
	return r.Storage.Write(ctx, key, data)
}

func TestSaveMetadataSkipsAnUnchangedDocument(t *testing.T) {
	dir := t.TempDir()
	first := newBaseURLSyncer(t, dir, "")
	first.newComponents = []lvfs.Component{baseURLTestComponent()}
	require.NoError(t, first.SaveMetadata(context.Background()))

	reload := func(baseURL string) (*FirmirrorSyncer, *writeRecorder) {
		syncer := newBaseURLSyncer(t, dir, baseURL)
		recorder := &writeRecorder{Storage: syncer.Storage}
		syncer.Storage = recorder
		require.NoError(t, syncer.LoadMetadata(context.Background()))
		return syncer, recorder
	}

	t.Run("NothingNewWritesNothing", func(t *testing.T) {
		syncer, recorder := reload("")
		require.NoError(t, syncer.SaveMetadata(context.Background()))
		assert.Empty(t, recorder.keys, "an unchanged document must not be written or signed again")
	})

	t.Run("AChangedDocumentIsWritten", func(t *testing.T) {
		syncer, recorder := reload("https://mirror.example.com/fwupd")
		require.NoError(t, syncer.SaveMetadata(context.Background()))
		assert.ElementsMatch(t, []string{"metadata.xml.zst.jcat", "metadata.xml.zst"}, recorder.keys)
	})
}

func TestSaveMetadataWithNothingToPublishWritesNothing(t *testing.T) {
	dir := t.TempDir()
	syncer := newBaseURLSyncer(t, dir, "")
	recorder := &writeRecorder{Storage: syncer.Storage}
	syncer.Storage = recorder

	require.NoError(t, syncer.LoadMetadata(context.Background()))
	require.NoError(t, syncer.SaveMetadata(context.Background()))
	assert.Empty(t, recorder.keys, "an empty run must not replace the metadata with an empty document")
}

// failOnce fails the first write of a key, as a pod dying between the JCAT and
// the metadata would.
type failOnce struct {
	Storage
	key    string
	failed bool
}

func (s *failOnce) Write(ctx context.Context, key string, data io.Reader) error {
	if key == s.key && !s.failed {
		s.failed = true
		return errors.New("storage went away")
	}
	return s.Storage.Write(ctx, key, data)
}

func TestSaveMetadataRepairsAJCATLeftBehindByAnInterruptedWrite(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	first := newBaseURLSyncer(t, dir, "")
	first.newComponents = []lvfs.Component{baseURLTestComponent()}
	require.NoError(t, first.SaveMetadata(ctx))

	// A refresh mirrors something new, writes its JCAT, and dies before the metadata.
	other := baseURLTestComponent()
	other.ID = "com.test.firmware2"
	interrupted := newBaseURLSyncer(t, dir, "")
	interrupted.Storage = &failOnce{Storage: interrupted.Storage, key: "metadata.xml.zst"}
	require.NoError(t, interrupted.LoadMetadata(ctx))
	interrupted.newComponents = []lvfs.Component{other}
	require.Error(t, interrupted.SaveMetadata(ctx))

	reload := func() (*FirmirrorSyncer, *writeRecorder) {
		syncer := newBaseURLSyncer(t, dir, "")
		recorder := &writeRecorder{Storage: syncer.Storage}
		syncer.Storage = recorder
		require.NoError(t, syncer.LoadMetadata(ctx))
		return syncer, recorder
	}

	// Nothing new, and the document is the stored one, but its JCAT is another's.
	syncer, recorder := reload()
	require.NoError(t, syncer.SaveMetadata(ctx))
	assert.ElementsMatch(t, []string{"metadata.xml.zst.jcat", "metadata.xml.zst"}, recorder.keys,
		"a JCAT that is not the stored metadata's must be replaced even though the document is unchanged")

	syncer, recorder = reload()
	require.NoError(t, syncer.SaveMetadata(ctx))
	assert.Empty(t, recorder.keys, "and once it matches, the document is left alone again")
}

func TestSaveMetadataWritesTheJCATWhenItIsMissing(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	first := newBaseURLSyncer(t, dir, "")
	first.newComponents = []lvfs.Component{baseURLTestComponent()}
	require.NoError(t, first.SaveMetadata(ctx))
	require.NoError(t, os.Remove(filepath.Join(dir, "metadata.xml.zst.jcat")))

	syncer := newBaseURLSyncer(t, dir, "")
	recorder := &writeRecorder{Storage: syncer.Storage}
	syncer.Storage = recorder
	require.NoError(t, syncer.LoadMetadata(ctx))
	require.NoError(t, syncer.SaveMetadata(ctx))

	assert.Contains(t, recorder.keys, "metadata.xml.zst.jcat")
}

func TestSaveMetadataSignsAnUnchangedDocumentWhenSigningIsTurnedOn(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	first := newBaseURLSyncer(t, dir, "")
	first.newComponents = []lvfs.Component{baseURLTestComponent()}
	require.NoError(t, first.SaveMetadata(ctx))

	reload := func(signed bool) (*FirmirrorSyncer, *writeRecorder) {
		syncer := newBaseURLSyncer(t, dir, "")
		if signed {
			syncer.Config.Certificate = "/secrets/signing.cert"
			syncer.Config.PrivateKey = "/secrets/signing.key"
		}
		recorder := &writeRecorder{Storage: syncer.Storage}
		syncer.Storage = recorder
		require.NoError(t, syncer.LoadMetadata(ctx))
		return syncer, recorder
	}

	syncer, recorder := reload(true)
	require.NoError(t, syncer.SaveMetadata(ctx))
	assert.ElementsMatch(t, []string{"metadata.xml.zst.jcat", "metadata.xml.zst"}, recorder.keys,
		"a checksum-only JCAT must be replaced by a signed one once signing is configured")

	syncer, recorder = reload(true)
	require.NoError(t, syncer.SaveMetadata(ctx))
	assert.Empty(t, recorder.keys, "and once it is signed, the document is left alone again")

	syncer, recorder = reload(false)
	require.NoError(t, syncer.SaveMetadata(ctx))
	assert.Empty(t, recorder.keys, "a signed JCAT is not replaced by a run that does not sign")
}

func TestSaveMetadataLeavesAlonePublishedMeanwhile(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	first := newBaseURLSyncer(t, dir, "")
	first.newComponents = []lvfs.Component{baseURLTestComponent()}
	require.NoError(t, first.SaveMetadata(ctx))

	// A run that mirrors nothing loads the document and works for a while.
	slow := newBaseURLSyncer(t, dir, "")
	recorder := &writeRecorder{Storage: slow.Storage}
	slow.Storage = recorder
	require.NoError(t, slow.LoadMetadata(ctx))

	// Another one publishes new firmware in the meantime.
	other := baseURLTestComponent()
	other.ID = "com.test.firmware2"
	faster := newBaseURLSyncer(t, dir, "")
	require.NoError(t, faster.LoadMetadata(ctx))
	faster.newComponents = []lvfs.Component{other}
	require.NoError(t, faster.SaveMetadata(ctx))

	require.NoError(t, slow.SaveMetadata(ctx))
	assert.Empty(t, recorder.keys, "a run with nothing to publish must not replace what another run published")
	assert.Len(t, readSavedMetadata(t, dir).Component, 2)
}

// publishedMeanwhile loads a run that mirrors nothing, then has another run
// publish firmware2.cab, and returns the first, to be saved.
func publishedMeanwhile(t *testing.T, dir string, configure func(*FirmirrorSyncer)) *FirmirrorSyncer {
	t.Helper()
	ctx := context.Background()
	first := newBaseURLSyncer(t, dir, "")
	first.newComponents = []lvfs.Component{baseURLTestComponent()}
	require.NoError(t, first.SaveMetadata(ctx))

	slow := newBaseURLSyncer(t, dir, "")
	configure(slow)
	require.NoError(t, slow.LoadMetadata(ctx))

	other := baseURLTestComponent()
	other.ID = "com.test.firmware2"
	other.Releases[0].Artifacts[0].Location = "firmware2.cab"
	require.NoError(t, os.WriteFile(filepath.Join(dir, "firmware2.cab"), []byte("cab"), 0o644))
	faster := newBaseURLSyncer(t, dir, "")
	require.NoError(t, faster.LoadMetadata(ctx))
	faster.newComponents = []lvfs.Component{other}
	require.NoError(t, faster.SaveMetadata(ctx))
	return slow
}

func TestSaveMetadataDoesNotReclaimThePackagesOfARunPublishedMeanwhile(t *testing.T) {
	dir := t.TempDir()
	slow := publishedMeanwhile(t, dir, func(s *FirmirrorSyncer) {
		s.Config.CleanupUnreferencedPackages = true
		s.Storage = &cleanupStorage{LocalStorage: s.Storage.(*LocalStorage)}
	})

	require.NoError(t, slow.SaveMetadata(context.Background()))
	assert.FileExists(t, filepath.Join(dir, "firmware2.cab"), "the document this run built does not reference it, the stored one does")
}

func TestSaveMetadataLeavesAnUnsignedDocumentPublishedMeanwhileEvenWhenSigning(t *testing.T) {
	dir := t.TempDir()
	var recorder *writeRecorder
	slow := publishedMeanwhile(t, dir, func(s *FirmirrorSyncer) {
		s.Config.Certificate = "/secrets/signing.cert"
		s.Config.PrivateKey = "/secrets/signing.key"
		recorder = &writeRecorder{Storage: s.Storage}
		s.Storage = recorder
	})

	require.NoError(t, slow.SaveMetadata(context.Background()))
	assert.Empty(t, recorder.keys, "a run that signs must not replace another run's document with its own")
	assert.Len(t, readSavedMetadata(t, dir).Component, 2)
}

func TestSaveMetadataSignsIntoAFreshJCAT(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	syncer := newBaseURLSyncer(t, dir, "")
	// What an earlier run that was killed while signing left in the cache.
	leftover := filepath.Join(syncer.Config.CacheDir, "metadata.xml.zst.jcat")
	require.NoError(t, os.WriteFile(leftover, []byte("stale"), 0o644))
	syncer.newComponents = []lvfs.Component{baseURLTestComponent()}

	require.NoError(t, syncer.SaveMetadata(ctx))

	jcat, err := syncer.readStoredJCAT(ctx)
	require.NoError(t, err)
	assert.False(t, jcat.hasChecksum("stale"), "a checksum from a leftover JCAT would make clients reject the metadata")
	assert.True(t, jcat.hasChecksum(fileChecksumOfStored(t, dir)))
}

func fileChecksumOfStored(t *testing.T, dir string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, "metadata.xml.zst"))
	require.NoError(t, err)
	return fileChecksum(data)
}
