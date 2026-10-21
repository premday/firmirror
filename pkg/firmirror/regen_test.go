package firmirror

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/klauspost/compress/zstd"
	"github.com/premday/firmirror/pkg/lvfs"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRegenerateMetadataPublishesADocumentThatDidNotChange(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	refresh(t, dir, definitions("10.0"), versioned("10.0", "a.cab"))

	syncer := newRingsSyncer(t, dir, definitions("10.0"))
	recorder := &writeRecorder{Storage: syncer.Storage}
	syncer.Storage = recorder
	require.NoError(t, syncer.LoadMetadata(ctx))
	require.NoError(t, syncer.RegenerateMetadata(ctx))

	assert.ElementsMatch(t, []string{"metadata.xml.zst.jcat", "metadata.xml.zst"}, recorder.keys,
		"a new signing key is no change of the document, and a refresh would leave it alone")
}

func TestRegenerateMetadataSignsWithTheKeysItIsGiven(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	refresh(t, dir, definitions("10.0"), versioned("10.0", "a.cab"))

	signed := newRingsSyncer(t, dir, definitions("10.0"))
	signed.Config.Certificate = "/secrets/signing.cert"
	signed.Config.PrivateKey = "/secrets/signing.key"
	require.NoError(t, signed.LoadMetadata(ctx))
	require.NoError(t, signed.RegenerateMetadata(ctx))

	jcat, err := signed.readStoredJCAT(ctx)
	require.NoError(t, err)
	assert.True(t, jcat.isSigned())

	unsigned := newRingsSyncer(t, dir, definitions("10.0"))
	require.NoError(t, unsigned.LoadMetadata(ctx))
	assert.ErrorContains(t, unsigned.RegenerateMetadata(ctx), "stored JCAT is signed",
		"checksums alone would be rejected by the clients that require a signature")
}

func TestRegenerateMetadataNeedsMetadata(t *testing.T) {
	syncer := newRingsSyncer(t, t.TempDir(), definitions("10.0"))
	require.NoError(t, syncer.LoadMetadata(context.Background()))

	assert.ErrorContains(t, syncer.RegenerateMetadata(context.Background()), "run a refresh first")
}

func TestReadMetadataFromAPathAndAURL(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	refresh(t, dir, definitions("10.0", "10.0"), versioned("10.0", "a.cab"), versioned("20.0", "b.cab"))
	path := filepath.Join(dir, "metadata.xml.zst")
	server := httptest.NewServer(http.FileServer(http.Dir(dir)))
	defer server.Close()

	for _, source := range []string{path, server.URL + "/metadata.xml.zst"} {
		components, err := ReadMetadata(ctx, source, false)
		require.NoError(t, err, source)
		require.Len(t, components, 1, "the pieces it was published in are put together")
		assert.Len(t, components[0].Releases, 2)
	}

	_, err := ReadMetadata(ctx, server.URL+"/missing", false)
	assert.Error(t, err)
}

func TestRegenerationDropsWhatTheStoredDocumentHasAndTheArgumentsDoNot(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	published := newRingsSyncer(t, dir, definitions("10.0"))
	published.Config.FirmwareBaseURL = "https://mirror.example.com/fwupd/"
	require.NoError(t, published.LoadMetadata(ctx))
	published.newComponents = []lvfs.Component{versioned("10.0", "a.cab")}
	require.NoError(t, published.SaveMetadata(ctx))

	forgetful := newRingsSyncer(t, dir, nil)
	require.NoError(t, forgetful.LoadMetadata(ctx))
	assert.Error(t, forgetful.baseURLDropped())
	assert.ErrorContains(t, forgetful.RegenerateMetadata(ctx), "take the base URL out")
	forgetful.Config.Force = true
	assert.NoError(t, forgetful.RegenerateMetadata(ctx))

	complete := newRingsSyncer(t, dir, definitions("10.0"))
	complete.Config.FirmwareBaseURL = "https://mirror.example.com/fwupd/"
	require.NoError(t, complete.LoadMetadata(ctx))
	assert.NoError(t, complete.baseURLDropped())
}

func TestNoMetadataTellsAnUnversionedDocumentFromNone(t *testing.T) {
	syncer := newRingsSyncer(t, t.TempDir(), nil)
	assert.ErrorContains(t, syncer.noMetadata(), "run a refresh first")

	syncer.stored = fingerprint{file: "x"}
	assert.ErrorContains(t, syncer.noMetadata(), "schema version 0")
}

func TestRegenerateMetadataDoesNotReplaceWhatWasPublishedMeanwhile(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	refresh(t, dir, definitions("10.0"), versioned("10.0", "a.cab"))
	regen := newRingsSyncer(t, dir, definitions("10.0"))
	require.NoError(t, regen.LoadMetadata(ctx))

	refresh(t, dir, definitions("10.0"), versioned("20.0", "b.cab"))

	assert.ErrorContains(t, regen.RegenerateMetadata(ctx), "run it again")
	assert.Len(t, publishedTags(t, dir), 2, "the release the refresh published is still there")
}

func TestReadMetadataRefusesAnotherSchema(t *testing.T) {
	dir := t.TempDir()
	refresh(t, dir, definitions("10.0"), versioned("10.0", "a.cab"))
	path := filepath.Join(dir, "metadata.xml.zst")
	compressed, err := os.ReadFile(path)
	require.NoError(t, err)
	data, err := decompress(compressed)
	require.NoError(t, err)
	older := strings.Replace(string(data), fmt.Sprintf(`schema_version="%d"`, lvfs.MetadataSchemaVersion), `schema_version="1"`, 1)
	require.NotEqual(t, string(data), older)
	encoder, err := zstd.NewWriter(nil)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, encoder.EncodeAll([]byte(older), nil), 0o644))

	_, err = ReadMetadata(context.Background(), path, false)

	assert.ErrorContains(t, err, "schema version 1")
}

func TestBuildingTheDocumentLeavesTheMirroredComponentsAlone(t *testing.T) {
	syncer := newRingsSyncer(t, t.TempDir(), definitions("10.0"))
	mirrored := versioned("10.0", "a.cab")
	syncer.newComponents = []lvfs.Component{mirrored}

	syncer.buildDocument(false)

	assert.Empty(t, mirrored.Releases[0].Tags, "the tags are on the document's releases, not on the ones the run holds")
}

func TestReadStoredComponents(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	storage := newRingsSyncer(t, dir, nil).Storage

	_, err := ReadStoredComponents(ctx, storage)
	assert.ErrorContains(t, err, "run a refresh first")

	refresh(t, dir, definitions("10.0"), versioned("10.0", "a.cab"), versioned("20.0", "b.cab"))
	components, err := ReadStoredComponents(ctx, storage)
	require.NoError(t, err)
	require.Len(t, components, 1)
	assert.Len(t, components[0].Releases, 2)
}

func TestReadMetadataSaysNoTokenOfTheURLItFailedOn(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("not metadata")) }))
	defer server.Close()

	_, err := ReadMetadata(context.Background(), server.URL+"/metadata.xml.zst?token=SECRET", false)

	require.Error(t, err)
	assert.NotContains(t, err.Error(), "SECRET")
}
