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
	"github.com/premday/firmirror/pkg/bkc"
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

func TestARingTheDefinitionsNameNoVersionForCannotBePublishedAndIsRefused(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	refresh(t, dir, definitions("10.0", "10.0"), versioned("10.0", "a.cab"))

	// beta is still declared, but no entry names a version for it, so its tag
	// could not be published and its hosts would be taken off it.
	syncer := newRingsSyncer(t, dir, definitions("10.0"))
	require.NoError(t, syncer.LoadMetadata(ctx))

	assert.ErrorContains(t, syncer.Preflight(ctx), "no longer be published")
	assert.ErrorContains(t, syncer.RegenerateMetadata(ctx), "no longer be published")
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
	assert.ErrorContains(t, forgetful.RegenerateMetadata(ctx), "no longer be published")
	forgetful.Config.Force = true
	assert.ErrorContains(t, forgetful.RegenerateMetadata(ctx), "names its packages by URL", "forcing the tags out does not drop the base URL")
	forgetful.Config.DropBaseURL = true
	assert.NoError(t, forgetful.RegenerateMetadata(ctx), "each override is its own")

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

func TestPreflightRefusesWhatThePublishWouldBeforeAnythingIsMirrored(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	refresh(t, dir, definitions("10.0"), versioned("10.0", "a.cab"))

	forgetful := newRingsSyncer(t, dir, nil)
	require.NoError(t, forgetful.LoadMetadata(ctx))
	assert.ErrorContains(t, forgetful.Preflight(ctx), "would no longer be published")
	forgetful.Config.Force = true
	assert.NoError(t, forgetful.Preflight(ctx))

	given := newRingsSyncer(t, dir, definitions("10.0"))
	require.NoError(t, given.LoadMetadata(ctx))
	assert.NoError(t, given.Preflight(ctx))
}

func TestAPartialSaveAfterADiscardedDocumentIsNotRefusedForItsTags(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	refresh(t, dir, definitions("10.0"), versioned("10.0", "a.cab"))

	syncer := newRingsSyncer(t, dir, definitions("20.0"))
	require.NoError(t, syncer.LoadMetadata(ctx))
	syncer.existingMetadata = nil // as when its schema version was not the current one
	syncer.newComponents = []lvfs.Component{versioned("10.0", "a.cab")}

	assert.NoError(t, syncer.SaveMetadata(ctx), "what it replaces was not carried, so nothing is unserved by it")
}

func TestWhatWasPublishedIsWhatTheNextWriteIsComparedWith(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	syncer := newRingsSyncer(t, dir, definitions("10.0"))
	require.NoError(t, syncer.LoadMetadata(ctx))
	syncer.newComponents = []lvfs.Component{versioned("10.0", "a.cab")}
	require.NoError(t, syncer.SaveMetadata(ctx))

	assert.Equal(t, []string{"firmirror-dev"}, syncer.facts.tags)
}

func TestRefusesToDropAnyTagThatWasPublished(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	refresh(t, dir, definitions("10.0", "10.0"), versioned("10.0", "a.cab"))

	renamed := definitions("10.0", "10.0")
	renamed.TagPrefix = "other"
	syncer := newRingsSyncer(t, dir, renamed)
	require.NoError(t, syncer.LoadMetadata(ctx))
	syncer.newComponents = []lvfs.Component{versioned("20.0", "b.cab")}

	err := syncer.SaveMetadata(ctx)

	assert.ErrorContains(t, err, "firmirror-beta, firmirror-dev", "a changed prefix leaves every host on a ring that is not published")
	syncer.Config.Force = true
	assert.NoError(t, syncer.SaveMetadata(ctx))
}

func TestAnEmptyTagsElementIsNoRingTag(t *testing.T) {
	stored, err := parseStored(compressTestDocument(t, `<components origin="firmirror" schema_version="3"><component><id>a</id><tags></tags><tags><tag namespace="other">x</tag></tags></component></components>`))

	require.NoError(t, err)
	assert.Empty(t, stored.facts.tags, "only tags in the namespace fwupd reads are ring tags")
}

func compressTestDocument(t *testing.T, document string) []byte {
	t.Helper()
	encoder, err := zstd.NewWriter(nil)
	require.NoError(t, err)
	return encoder.EncodeAll([]byte(document), nil)
}

func TestBuildingTheDocumentLeavesTheMirroredComponentsAlone(t *testing.T) {
	syncer := newRingsSyncer(t, t.TempDir(), definitions("10.0"))
	mirrored := versioned("10.0", "a.cab")
	syncer.newComponents = []lvfs.Component{mirrored}

	syncer.buildDocument(false)

	assert.Empty(t, mirrored.Releases[0].Tags, "the tags are on the document's releases, not on the ones the run holds")
}

func TestPreflightRefusesAStoredTagNoRingCouldPublish(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	refresh(t, dir, definitions("10.0", "10.0"), versioned("10.0", "a.cab"))

	renamed := definitions("10.0", "10.0")
	renamed.TagPrefix = "other"
	changed := newRingsSyncer(t, dir, renamed)
	require.NoError(t, changed.LoadMetadata(ctx))
	assert.ErrorContains(t, changed.Preflight(ctx), "firmirror-beta, firmirror-dev", "a changed prefix is known before anything is mirrored")

	fewer := newRingsSyncer(t, dir, bkc.New([]string{"dev"}))
	require.NoError(t, fewer.LoadMetadata(ctx))
	assert.ErrorContains(t, fewer.Preflight(ctx), "firmirror-beta")

	same := newRingsSyncer(t, dir, definitions("20.0", "20.0"))
	require.NoError(t, same.LoadMetadata(ctx))
	assert.NoError(t, same.Preflight(ctx), "a ring that moves to another version still publishes its tag")
}

func TestPreflightHoldsARemirrorToTheTagsOfWhatItDiscarded(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	refresh(t, dir, definitions("10.0"), versioned("10.0", "a.cab"))

	syncer := newRingsSyncer(t, dir, nil)
	require.NoError(t, syncer.LoadMetadata(ctx))
	syncer.existingMetadata = nil // as when its schema version was not the current one

	assert.ErrorContains(t, syncer.Preflight(ctx), "would no longer be published", "the definitions alone say which tags are published, whatever the re-mirror got to")
	syncer.Config.Force = true
	assert.NoError(t, syncer.Preflight(ctx))
}

func TestARingWhoseVersionsTheMirrorLostDoesNotStopThePublish(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	refresh(t, dir, definitions("10.0", "10.0"), versioned("10.0", "a.cab"))

	lost := definitions("10.0", "9.9")
	syncer := newRingsSyncer(t, dir, lost)
	require.NoError(t, syncer.LoadMetadata(ctx))
	syncer.newComponents = []lvfs.Component{versioned("20.0", "b.cab")}

	assert.NoError(t, syncer.Preflight(ctx))
	assert.NoError(t, syncer.SaveMetadata(ctx), "its hosts stay where they are, as for any version the mirror lost")
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

func TestADocumentWithNoRingTagAtAllIsRefusedWhenTheStoredHasSome(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	refresh(t, dir, definitions("10.0", "10.0"), versioned("10.0", "a.cab"))

	elsewhere := definitions("7.7", "7.7")
	syncer := newRingsSyncer(t, dir, elsewhere)
	require.NoError(t, syncer.LoadMetadata(ctx))
	syncer.newComponents = []lvfs.Component{versioned("20.0", "b.cab")}

	assert.NoError(t, syncer.Preflight(ctx), "the rings could publish their tags, which only the built document tells")
	assert.ErrorContains(t, syncer.SaveMetadata(ctx), "no ring tag at all")
}

func TestLocationsAreURLsWhenTheirArtifactsAre(t *testing.T) {
	stored, err := parseStored(compressTestDocument(t, `<components origin="firmirror" schema_version="3"><component><id>a</id><releases><release version="1" location="a.cab"><artifacts><artifact type="binary"><location>https://mirror.example.com/a.cab</location></artifact></artifacts></release></releases></component></components>`))

	require.NoError(t, err)
	assert.True(t, stored.facts.absolute)
}

func TestARefreshWithoutTheBaseURLItWasPublishedWithIsRefused(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	published := newRingsSyncer(t, dir, definitions("10.0"))
	published.Config.FirmwareBaseURL = "https://mirror.example.com/fwupd/"
	require.NoError(t, published.LoadMetadata(ctx))
	published.newComponents = []lvfs.Component{versioned("10.0", "a.cab")}
	require.NoError(t, published.SaveMetadata(ctx))

	forgetful := newRingsSyncer(t, dir, definitions("10.0"))
	require.NoError(t, forgetful.LoadMetadata(ctx))
	forgetful.newComponents = []lvfs.Component{versioned("20.0", "b.cab")}

	assert.ErrorContains(t, forgetful.Preflight(ctx), "base URL")
	assert.ErrorContains(t, forgetful.SaveMetadata(ctx), "base URL")
	forgetful.Config.Force = true
	assert.ErrorContains(t, forgetful.SaveMetadata(ctx), "base URL", "forcing a tag out does not also drop the base URL")
	forgetful.Config.DropBaseURL = true
	assert.NoError(t, forgetful.SaveMetadata(ctx))
}
