package main

import (
	"bytes"
	"context"
	"encoding/xml"
	"os"
	"path/filepath"
	"testing"

	"github.com/klauspost/compress/zstd"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/premday/firmirror/pkg/bkc"
	"github.com/premday/firmirror/pkg/firmirror"
	"github.com/premday/firmirror/pkg/lvfs"
)

const guid = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"

// storeTagged stores metadata whose component is served by the dev and beta rings.
func storeTagged(t *testing.T, dir string) {
	t.Helper()
	component := lvfs.Component{
		Type: "firmware", ID: "com.test.nic", Name: "NIC",
		Provides: []lvfs.Firmware{{Type: "flashed", Text: guid}},
		Tags: &lvfs.Tags{Tag: []lvfs.Tag{
			{Namespace: lvfs.TagNamespace, Value: "firmirror-dev"},
			{Namespace: lvfs.TagNamespace, Value: "firmirror-beta"},
		}},
		Releases: []lvfs.Release{{Version: "1.0", Date: "2026-01-01", Location: "nic.cab"}},
	}
	document, err := xml.Marshal(&lvfs.Components{Origin: "firmirror", SchemaVersion: lvfs.MetadataSchemaVersion, Component: []lvfs.Component{component}})
	require.NoError(t, err)
	var compressed bytes.Buffer
	encoder, err := zstd.NewWriter(&compressed)
	require.NoError(t, err)
	_, err = encoder.Write(document)
	require.NoError(t, err)
	require.NoError(t, encoder.Close())
	require.NoError(t, os.WriteFile(filepath.Join(dir, "metadata.xml.zst"), compressed.Bytes(), 0o644))
}

func ringsServing(rings ...string) *bkc.File {
	file := bkc.New(rings)
	versions := map[string]string{}
	for _, ring := range rings {
		versions[ring] = "1.0"
	}
	file.Components = []bkc.Entry{{ID: "com.test.nic", Platform: bkc.Platform([]string{guid}), Versions: versions}}
	return file
}

func TestARefreshFallsBackToTheRingsInUseWhenTheNewOnesWouldBeRefused(t *testing.T) {
	withArgs(t)
	t.Chdir(t.TempDir())
	dir := t.TempDir()
	args.OutputDir = dir
	storeTagged(t, dir)
	storage, err := openStorage(context.Background())
	require.NoError(t, err)
	inUse, noBeta := ringsServing("dev", "beta"), ringsServing("dev")

	_, err = refresh(context.Background(), storage, noBeta, refreshRun{})
	assert.ErrorIs(t, err, firmirror.ErrRefused, "without ones to fall back to, it is refused")

	outcome, err := refresh(context.Background(), storage, noBeta, refreshRun{inUse: inUse})

	require.NoError(t, err)
	assert.Same(t, inUse, outcome.rings, "what was published is what was in use")
	assert.True(t, outcome.holdsMetadata)
}

func TestARefreshWithRingsThatTagNothingPublishesWithTheOnesInUse(t *testing.T) {
	withArgs(t)
	t.Chdir(t.TempDir())
	dir := t.TempDir()
	args.OutputDir = dir
	storeTagged(t, dir)
	storage, err := openStorage(context.Background())
	require.NoError(t, err)
	inUse := ringsServing("dev", "beta")
	elsewhere := ringsServing("dev", "beta")
	elsewhere.Components[0].Platform = bkc.Platform([]string{"another-guid"})

	_, err = refresh(context.Background(), storage, elsewhere, refreshRun{})
	assert.ErrorIs(t, err, firmirror.ErrRingsRefused, "nothing to fall back to")

	outcome, err := refresh(context.Background(), storage, elsewhere, refreshRun{inUse: inUse})

	require.NoError(t, err)
	assert.Same(t, inUse, outcome.rings)
	assert.True(t, outcome.published, "so the daemon takes them as the ones in force")
}
