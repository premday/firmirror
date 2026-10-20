package firmirror

import (
	"context"
	"io"
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
