package firmirror

import (
	"context"
	"errors"
	"fmt"
	"io"
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

// blipOnMetadata is a storage that cannot tell whether the metadata exists the
// first times it is asked.
type blipOnMetadata struct {
	Storage
	failures int
}

func (b *blipOnMetadata) Exists(ctx context.Context, key string) (bool, error) {
	if key == metadataKey && b.failures > 0 {
		b.failures--
		return false, errors.New("storage unavailable")
	}
	return b.Storage.Exists(ctx, key)
}

func TestARegenerationSurvivesAStorageThatBlipsWhileItChecksForAnotherPublisher(t *testing.T) {
	quickJCATRetries(t)
	ctx := context.Background()
	dir := t.TempDir()
	refresh(t, dir, definitions("10.0"), versioned("10.0", "a.cab"))

	syncer := newRingsSyncer(t, dir, definitions("10.0"))
	require.NoError(t, syncer.LoadMetadata(ctx))
	syncer.Storage = &blipOnMetadata{Storage: syncer.Storage, failures: 2}
	assert.NoError(t, syncer.RegenerateMetadata(ctx), "twice again is what it tries")

	syncer = newRingsSyncer(t, dir, definitions("10.0"))
	require.NoError(t, syncer.LoadMetadata(ctx))
	syncer.Storage = &blipOnMetadata{Storage: syncer.Storage, failures: 3}
	assert.ErrorContains(t, syncer.RegenerateMetadata(ctx), "could not check whether the metadata was published")
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

func TestDefinitionsAreReadAgainBeforeThePublish(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	syncer := newRingsSyncer(t, dir, definitions("10.0"))
	syncer.Config.ReloadBKC = func(context.Context) (*bkc.File, error) { return definitions("10.0", "10.0"), nil }
	require.NoError(t, syncer.LoadMetadata(ctx))
	syncer.newComponents = []lvfs.Component{versioned("10.0", "a.cab")}

	require.NoError(t, syncer.SaveMetadata(ctx))

	assert.Equal(t, []string{"firmirror-beta", "firmirror-dev"}, publishedTags(t, dir)["10.0"], "the promotion committed during the run is not reverted")
}

func TestDefinitionsThatCannotBeReadAgainAreTheOnesTheRunStartedWith(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	syncer := newRingsSyncer(t, dir, definitions("10.0"))
	syncer.Config.ReloadBKC = func(context.Context) (*bkc.File, error) { return nil, errors.New("unreachable") }
	require.NoError(t, syncer.LoadMetadata(ctx))
	syncer.newComponents = []lvfs.Component{versioned("10.0", "a.cab")}

	require.NoError(t, syncer.SaveMetadata(ctx), "the run has done its work")

	assert.Equal(t, []string{"firmirror-dev"}, publishedTags(t, dir)["10.0"])
}

func TestBuildingTheDocumentLeavesTheMirroredComponentsAlone(t *testing.T) {
	syncer := newRingsSyncer(t, t.TempDir(), definitions("10.0"))
	mirrored := versioned("10.0", "a.cab")
	syncer.newComponents = []lvfs.Component{mirrored}

	_, _, _ = syncer.buildDocument(false)

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

func TestDefinitionsReadAgainThatWouldDropATagAreNotUsed(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	refresh(t, dir, definitions("10.0", "10.0"), versioned("10.0", "a.cab"))
	syncer := newRingsSyncer(t, dir, definitions("10.0", "10.0"))
	syncer.Config.ReloadBKC = func(context.Context) (*bkc.File, error) { return bkc.New([]string{"dev", "beta"}), nil }
	require.NoError(t, syncer.LoadMetadata(ctx))
	syncer.newComponents = []lvfs.Component{versioned("20.0", "b.cab")}

	require.NoError(t, syncer.SaveMetadata(ctx), "the run's work is published, with the definitions that passed the first check")

	assert.Equal(t, []string{"firmirror-beta", "firmirror-dev"}, publishedTags(t, dir)["10.0"])
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

func TestAnUnchangedDocumentWhoseJCATCannotBeReadIsLeftAlone(t *testing.T) {
	quickJCATRetries(t)
	ctx := context.Background()
	dir := t.TempDir()
	refresh(t, dir, definitions("10.0"), versioned("10.0", "a.cab"))
	syncer := newRingsSyncer(t, dir, definitions("10.0"))
	require.NoError(t, syncer.LoadMetadata(ctx))
	syncer.Storage = failingJCAT{syncer.Storage}

	assert.NoError(t, syncer.SaveMetadata(ctx), "nothing changed, and what could not be read may be a signature")
}

func TestAnUnchangedDocumentWithAnEmptyJCATGetsItsJCATBack(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	refresh(t, dir, definitions("10.0"), versioned("10.0", "a.cab"))
	jcatPath := filepath.Join(dir, "metadata.xml.zst.jcat")
	require.NoError(t, os.WriteFile(jcatPath, nil, 0o644))
	syncer := newRingsSyncer(t, dir, definitions("10.0"))
	require.NoError(t, syncer.LoadMetadata(ctx))

	require.NoError(t, syncer.SaveMetadata(ctx))

	info, err := os.Stat(jcatPath)
	require.NoError(t, err)
	assert.NotZero(t, info.Size(), "clients that verify the JCAT reject the remote until it is replaced")
}

func TestAnUnchangedDocumentThatCannotBeCheckedDoesNotCleanUp(t *testing.T) {
	quickJCATRetries(t)
	ctx := context.Background()
	dir := t.TempDir()
	refresh(t, dir, definitions("10.0"), versioned("10.0", "a.cab"))
	syncer := newRingsSyncer(t, dir, definitions("10.0"))
	require.NoError(t, syncer.LoadMetadata(ctx))
	recorder := &deleteRecorder{Storage: failingJCAT{syncer.Storage}}
	syncer.Storage = recorder
	syncer.Config.CleanupUnreferencedPackages = true

	require.NoError(t, syncer.SaveMetadata(ctx))

	assert.Zero(t, recorder.listed, "another run may have published packages this document does not name")
}

// deleteRecorder is a storage that can clean up, and counts the listings a
// cleanup starts with.
type deleteRecorder struct {
	Storage
	listed int
}

func (d *deleteRecorder) List(context.Context, string) ([]string, error) {
	d.listed++
	return nil, nil
}

func (d *deleteRecorder) Delete(context.Context, string) error { return nil }

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

func TestDefinitionsReadAgainThatTagNothingTheMirrorHasAreNotUsed(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	refresh(t, dir, definitions("10.0", "10.0"), versioned("10.0", "a.cab"))
	elsewhere := bkc.New([]string{"dev", "beta", "stable"})
	elsewhere.Components = []bkc.Entry{{ID: "com.other", Versions: map[string]string{"dev": "1", "beta": "1"}}}
	syncer := newRingsSyncer(t, dir, definitions("10.0", "10.0"))
	syncer.Config.ReloadBKC = func(context.Context) (*bkc.File, error) { return elsewhere, nil }
	require.NoError(t, syncer.LoadMetadata(ctx))
	syncer.newComponents = []lvfs.Component{versioned("20.0", "b.cab")}

	require.NoError(t, syncer.SaveMetadata(ctx), "the starting definitions would have published fine")

	assert.Equal(t, []string{"firmirror-beta", "firmirror-dev"}, publishedTags(t, dir)["10.0"])
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

func TestRegenerateMetadataDoesNotNeedTheJCATToKnowItWasOvertaken(t *testing.T) {
	quickJCATRetries(t)
	ctx := context.Background()
	dir := t.TempDir()
	refresh(t, dir, definitions("10.0"), versioned("10.0", "a.cab"))
	regen := newRingsSyncer(t, dir, definitions("10.0"))
	signing := func(f *FirmirrorSyncer) {
		f.Config.Certificate, f.Config.PrivateKey = "/secrets/signing.cert", "/secrets/signing.key"
	}
	signing(regen)
	require.NoError(t, regen.LoadMetadata(ctx))

	refresh(t, dir, definitions("10.0"), versioned("20.0", "b.cab"))
	require.NoError(t, os.Remove(filepath.Join(dir, "metadata.xml.zst.jcat")))

	assert.ErrorContains(t, regen.RegenerateMetadata(ctx), "run it again", "the stored file is not the one that was read, whatever the JCAT says")
	assert.Len(t, publishedTags(t, dir), 2)
}

func TestRegenerateMetadataSaysWhenItCannotTellWhetherItWasOvertaken(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	refresh(t, dir, definitions("10.0"), versioned("10.0", "a.cab"))
	regen := newRingsSyncer(t, dir, definitions("10.0"))
	require.NoError(t, regen.LoadMetadata(ctx))
	regen.Storage = failingMetadata{regen.Storage}

	assert.ErrorContains(t, regen.RegenerateMetadata(ctx), "could not check whether the metadata was published")
}

func TestAChangedDocumentThatCannotBeCheckedIsNotReportedAsPublished(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	refresh(t, dir, definitions("10.0"), versioned("10.0", "a.cab"))
	refresh(t, dir, definitions("10.0"), versioned("20.0", "b.cab"))
	syncer := newRingsSyncer(t, dir, definitions("20.0"))
	require.NoError(t, syncer.LoadMetadata(ctx))
	syncer.Storage = failingMetadata{syncer.Storage}

	assert.ErrorContains(t, syncer.SaveMetadata(ctx), "run it again", "a promotion that was not published must fail the run, not pass for a success")
}

// failingMetadata is a storage whose metadata cannot be found out about.
type failingMetadata struct{ Storage }

func (f failingMetadata) Exists(ctx context.Context, key string) (bool, error) {
	if key == metadataKey {
		return false, errors.New("storage unavailable")
	}
	return f.Storage.Exists(ctx, key)
}

// unreadableMetadata is a storage whose metadata exists but cannot be read.
type unreadableMetadata struct{ Storage }

func (u unreadableMetadata) Read(ctx context.Context, key string) (io.ReadCloser, error) {
	if key == metadataKey {
		return nil, errors.New("storage unavailable")
	}
	return u.Storage.Read(ctx, key)
}

func TestAnUnchangedDocumentThatCannotBeHashedIsLeftAlone(t *testing.T) {
	quickJCATRetries(t)
	ctx := context.Background()
	dir := t.TempDir()
	refresh(t, dir, definitions("10.0"), versioned("10.0", "a.cab"))
	require.NoError(t, os.Remove(filepath.Join(dir, "metadata.xml.zst.jcat")))
	syncer := newRingsSyncer(t, dir, definitions("10.0"))
	require.NoError(t, syncer.LoadMetadata(ctx))
	syncer.Storage = unreadableMetadata{syncer.Storage}

	assert.NoError(t, syncer.SaveMetadata(ctx), "nothing changed, so there is nothing a retry would publish")
}

func TestAnUnchangedDocumentWhoseMetadataIsGoneIsPublishedAgain(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	refresh(t, dir, definitions("10.0"), versioned("10.0", "a.cab"))
	syncer := newRingsSyncer(t, dir, definitions("10.0"))
	require.NoError(t, syncer.LoadMetadata(ctx))
	require.NoError(t, os.Remove(filepath.Join(dir, "metadata.xml.zst")))

	require.NoError(t, syncer.SaveMetadata(ctx))

	assert.FileExists(t, filepath.Join(dir, "metadata.xml.zst"))
}

func TestAJCATOfThePublishedFileSavesReadingItAgain(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	refresh(t, dir, definitions("10.0"), versioned("10.0", "a.cab"))
	syncer := newRingsSyncer(t, dir, definitions("10.0"))
	require.NoError(t, syncer.LoadMetadata(ctx))
	counter := &readCounter{Storage: syncer.Storage}
	syncer.Storage = counter

	require.NoError(t, syncer.SaveMetadata(ctx))

	assert.Zero(t, counter.metadata, "the JCAT says the file is the one that was read")
}

// readCounter counts the reads of the metadata.
type readCounter struct {
	Storage
	metadata int
}

func (r *readCounter) Read(ctx context.Context, key string) (io.ReadCloser, error) {
	if key == metadataKey {
		r.metadata++
	}
	return r.Storage.Read(ctx, key)
}
