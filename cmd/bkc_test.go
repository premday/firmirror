package main

import (
	"bytes"
	"context"
	"encoding/xml"
	"io"
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

// writeMetadata publishes a component with releases of the given versions, the
// newest last, in a metadata file at the returned path.
func writeMetadata(t *testing.T, name string, versions ...string) string {
	t.Helper()
	component := lvfs.Component{
		Type: "firmware", ID: "com.test.nic", Name: "Test NIC\n\nModel: X1",
		Provides: []lvfs.Firmware{{Type: "flashed", Text: "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"}},
	}
	for i, version := range versions {
		component.Releases = append(component.Releases, lvfs.Release{Version: version, Date: "2026-01-0" + string(rune('1'+i))})
	}
	document, err := xml.Marshal(&lvfs.Components{Origin: "firmirror", SchemaVersion: lvfs.MetadataSchemaVersion, Component: []lvfs.Component{component}})
	require.NoError(t, err)
	var compressed bytes.Buffer
	encoder, err := zstd.NewWriter(&compressed)
	require.NoError(t, err)
	_, err = encoder.Write(document)
	require.NoError(t, err)
	require.NoError(t, encoder.Close())
	path := filepath.Join(t.TempDir(), name)
	require.NoError(t, os.WriteFile(path, compressed.Bytes(), 0o644))
	return path
}

func capture(t *testing.T, run func() error) (string, error) {
	t.Helper()
	stdout := os.Stdout
	reader, writer, err := os.Pipe()
	require.NoError(t, err)
	os.Stdout = writer
	restore := func() { os.Stdout = stdout }
	defer restore()

	// Read while it runs: what is printed beyond what a pipe holds would block it.
	captured := make(chan []byte)
	go func() {
		out, _ := io.ReadAll(reader)
		captured <- out
	}()
	runErr := run()
	restore()
	require.NoError(t, writer.Close())
	return string(<-captured), runErr
}

func promoteRing(t *testing.T, file, metadata, ring string, flags ...func()) (string, error) {
	t.Helper()
	args.BKC.File = file
	args.Rollout.Promote.Ring = ring
	args.Rollout.Promote.Metadata = metadata
	args.Rollout.Promote.DryRun = false
	args.Rollout.Promote.AllowDowngrade = false
	args.Rollout.Promote.Component = nil
	args.Rollout.Promote.Prune = false
	args.Rollout.Promote.AllowMassPrune = false
	for _, flag := range flags {
		flag()
	}
	return capture(t, func() error { return runBKCPromote(context.Background()) })
}

func TestPromoteCreatesTheFileAndMovesTheRingsOn(t *testing.T) {
	withArgs(t)
	metadata := writeMetadata(t, "metadata.xml.zst", "1.0", "2.0")
	file := filepath.Join(t.TempDir(), "bkc.yml")

	out, err := promoteRing(t, file, metadata, "dev")
	require.NoError(t, err)
	assert.Regexp(t, `com\.test\.nic\s+[0-9a-f]{16}\s+Test NIC Model: X1\s+-\s+2\.0`, out, "a name spread over lines is one cell")

	for _, ring := range []string{"beta", "preview", "stable"} {
		_, err = promoteRing(t, file, metadata, ring)
		require.NoError(t, err, ring)
	}

	loaded, err := bkc.Load(context.Background(), file, false)
	require.NoError(t, err)
	require.Len(t, loaded.Components, 1)
	assert.Equal(t, map[string]string{"dev": "2.0", "beta": "2.0", "preview": "2.0", "stable": "2.0"}, loaded.Components[0].Versions)
}

func TestPromoteNeedsTheRingBeforeIt(t *testing.T) {
	withArgs(t)
	metadata := writeMetadata(t, "metadata.xml.zst", "1.0")
	file := filepath.Join(t.TempDir(), "bkc.yml")

	out, err := promoteRing(t, file, metadata, "beta")

	require.NoError(t, err)
	assert.NoFileExists(t, file, "there is nothing to promote and nothing to write")
	assert.NotContains(t, out, "com.test.nic")
}

func TestPromoteDryRunWritesNothing(t *testing.T) {
	withArgs(t)
	metadata := writeMetadata(t, "metadata.xml.zst", "1.0")
	file := filepath.Join(t.TempDir(), "bkc.yml")

	out, err := promoteRing(t, file, metadata, "dev", func() { args.Rollout.Promote.DryRun = true })

	require.NoError(t, err)
	assert.Contains(t, out, "1.0")
	assert.NoFileExists(t, file)
}

func TestPromoteRefusesToDowngradeUnlessAllowed(t *testing.T) {
	withArgs(t)
	older := writeMetadata(t, "metadata.xml.zst", "1.0", "2.0")
	file := filepath.Join(t.TempDir(), "bkc.yml")
	definitions := bkc.New(bkc.DefaultRings)
	definitions.Components = []bkc.Entry{{
		ID: "com.test.nic", Platform: bkc.Platform([]string{"aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"}),
		Versions: map[string]string{"dev": "1.0", "beta": "2.0"},
	}}
	require.NoError(t, definitions.Write(file))
	before, err := os.ReadFile(file)
	require.NoError(t, err)

	_, err = promoteRing(t, file, older, "beta")
	assert.ErrorContains(t, err, "--allow-downgrade")
	after, err := os.ReadFile(file)
	require.NoError(t, err)
	assert.Equal(t, string(before), string(after), "a refused promotion writes nothing")

	_, err = promoteRing(t, file, older, "beta", func() { args.Rollout.Promote.AllowDowngrade = true })
	require.NoError(t, err)
	loaded, err := bkc.Load(context.Background(), file, false)
	require.NoError(t, err)
	assert.Equal(t, "1.0", loaded.Components[0].Versions["beta"])
}

func TestPromoteReadsTheStorageWhenGivenNoMetadata(t *testing.T) {
	withArgs(t)
	stored, err := os.ReadFile(writeMetadata(t, "metadata.xml.zst", "1.0", "2.0"))
	require.NoError(t, err)
	args.OutputDir = t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(args.OutputDir, "metadata.xml.zst"), stored, 0o644))
	file := filepath.Join(t.TempDir(), "bkc.yml")

	_, err = promoteRing(t, file, "", "dev")

	require.NoError(t, err)
	loaded, err := bkc.Load(context.Background(), file, false)
	require.NoError(t, err)
	require.Len(t, loaded.Components, 1)
	assert.Equal(t, "2.0", loaded.Components[0].Versions["dev"])
}

func TestPromoteKeepsTheTagPrefixOfTheFile(t *testing.T) {
	withArgs(t)
	metadata := writeMetadata(t, "metadata.xml.zst", "1.0")
	file := filepath.Join(t.TempDir(), "bkc.yml")
	definitions := bkc.New(bkc.DefaultRings)
	definitions.TagPrefix = "acme-eu"
	require.NoError(t, definitions.Write(file))

	_, err := promoteRing(t, file, metadata, "dev")

	require.NoError(t, err)
	loaded, err := bkc.Load(context.Background(), file, false)
	require.NoError(t, err)
	assert.Equal(t, "acme-eu-dev", loaded.Tag("dev"))
}

func TestPromoteReadsNoMoreThanTheFileLimit(t *testing.T) {
	withArgs(t)
	file := filepath.Join(t.TempDir(), "bkc.yml")
	require.NoError(t, os.WriteFile(file, []byte("# "+strings.Repeat("x", bkc.MaxFileSize)), 0o644))

	_, err := promoteRing(t, file, writeMetadata(t, "metadata.xml.zst", "1.0"), "dev")

	assert.ErrorContains(t, err, "too large")
}

func TestPromoteEditsAFileNotAURL(t *testing.T) {
	withArgs(t)
	_, err := promoteRing(t, "https://flavor.example.com/api/v1/firmirror_bkc", writeMetadata(t, "metadata.xml.zst", "1.0"), "dev")
	assert.ErrorContains(t, err, "has to be a path")

	_, err = promoteRing(t, "", "", "dev")
	assert.ErrorContains(t, err, "--bkc.file is required")
}

func TestPromoteRejectsAnUnknownRing(t *testing.T) {
	withArgs(t)
	_, err := promoteRing(t, filepath.Join(t.TempDir(), "bkc.yml"), writeMetadata(t, "metadata.xml.zst", "1.0"), "nowhere")
	assert.ErrorContains(t, err, `unknown ring "nowhere"`)
}

func TestBKCStatus(t *testing.T) {
	withArgs(t)
	file := filepath.Join(t.TempDir(), "bkc.yml")
	definitions := bkc.New([]string{"dev", "stable"})
	definitions.Components = []bkc.Entry{{ID: "com.test.nic", Name: "Test NIC\n\nModel: X1", Versions: map[string]string{"dev": "2.0", "stable": "1.0"}}}
	require.NoError(t, definitions.Write(file))
	args.BKC.File = file

	out, err := capture(t, func() error { return runBKCStatus(context.Background()) })

	require.NoError(t, err)
	assert.Regexp(t, `COMPONENT\s+PLATFORM\s+NAME\s+DEV\s+STABLE`, out)
	assert.Regexp(t, `com\.test\.nic\s+-\s+Test NIC Model: X1\s+2\.0\s+1\.0`, out)
	assert.Equal(t, 1, strings.Count(out, "com.test.nic"))

	args.Rollout.Status.Ring = "stable"
	out, err = capture(t, func() error { return runBKCStatus(context.Background()) })
	require.NoError(t, err)
	assert.NotContains(t, out, "DEV")

	definitions.Components = append(definitions.Components, bkc.Entry{ID: "com.test.old", Hold: true, Versions: map[string]string{"dev": "1.0"}})
	require.NoError(t, definitions.Write(file))
	args.Rollout.Status.Ring = ""
	out, err = capture(t, func() error { return runBKCStatus(context.Background()) })
	require.NoError(t, err)
	assert.Regexp(t, `com\.test\.old\s+-\s+held \(1\.0\)\s+held\b`, out, "a held component is served by no ring, and the versions it would serve are kept in view")

	args.Rollout.Status.Ring = "nowhere"
	assert.ErrorContains(t, runBKCStatus(context.Background()), `unknown ring "nowhere"`)

	args.BKC.File = ""
	assert.ErrorContains(t, runBKCStatus(context.Background()), "--bkc.file is required")
}

func TestPromoteWritesAnEntryItOnlyCompleted(t *testing.T) {
	withArgs(t)
	metadata := writeMetadata(t, "metadata.xml.zst", "1.0", "2.0")
	file := filepath.Join(t.TempDir(), "bkc.yml")
	definitions := bkc.New(bkc.DefaultRings)
	definitions.Components = []bkc.Entry{{ID: "com.test.nic", Versions: map[string]string{"dev": "2.0"}}}
	require.NoError(t, definitions.Write(file))

	_, err := promoteRing(t, file, metadata, "dev")

	require.NoError(t, err)
	loaded, err := bkc.Load(context.Background(), file, false)
	require.NoError(t, err)
	require.Len(t, loaded.Components, 1)
	assert.NotEmpty(t, loaded.Components[0].Platform, "no version moved, and the entry is still completed")
}

func TestPromotePrunesWhatTheMirrorLost(t *testing.T) {
	withArgs(t)
	metadata := writeMetadata(t, "metadata.xml.zst", "1.0", "2.0")
	file := filepath.Join(t.TempDir(), "bkc.yml")
	definitions := bkc.New(bkc.DefaultRings)
	definitions.Components = []bkc.Entry{
		{ID: "com.gone", Versions: map[string]string{"dev": "1.0"}},
		{ID: "com.test.nic", Versions: map[string]string{"dev": "1.0"}},
	}
	require.NoError(t, definitions.Write(file))

	_, err := promoteRing(t, file, metadata, "dev", func() { args.Rollout.Promote.Prune = true; args.Rollout.Promote.Component = []string{"com.test.nic"} })
	assert.ErrorContains(t, err, "--prune cannot be combined")

	out, err := promoteRing(t, file, metadata, "dev", func() { args.Rollout.Promote.Prune = true })
	require.NoError(t, err)
	assert.Regexp(t, `com\.gone\s+-.*removed`, out)
	loaded, err := bkc.Load(context.Background(), file, false)
	require.NoError(t, err)
	require.Len(t, loaded.Components, 1)
	assert.Equal(t, "com.test.nic", loaded.Components[0].ID)
}

func TestPromoteSaysWhenItPrunesAnEntryThatWasHeld(t *testing.T) {
	withArgs(t)
	metadata := writeMetadata(t, "metadata.xml.zst", "1.0", "2.0")
	file := filepath.Join(t.TempDir(), "bkc.yml")
	definitions := bkc.New(bkc.DefaultRings)
	definitions.Components = []bkc.Entry{
		{ID: "com.gone", Platform: bkc.Platform([]string{"g9"}), Hold: true, Versions: map[string]string{"dev": "1.0"}},
		{ID: "com.test.nic", Versions: map[string]string{"dev": "1.0"}},
	}
	require.NoError(t, definitions.Write(file))

	out, err := promoteRing(t, file, metadata, "dev", func() { args.Rollout.Promote.Prune = true })

	require.NoError(t, err)
	assert.Regexp(t, `com\.gone\s+\S+.*removed, the mirror does not have it, and it was held`, out)
}

func TestPromoteTakesAFileURLForAPath(t *testing.T) {
	withArgs(t)
	metadata := writeMetadata(t, "metadata.xml.zst", "1.0")
	file := filepath.Join(t.TempDir(), "bkc.yml")

	_, err := promoteRing(t, "file://"+file, metadata, "dev")

	require.NoError(t, err)
	assert.FileExists(t, file)
}

func TestPromoteRefusesToPruneMostOfTheFile(t *testing.T) {
	withArgs(t)
	metadata := writeMetadata(t, "metadata.xml.zst", "1.0")
	file := filepath.Join(t.TempDir(), "bkc.yml")
	definitions := bkc.New(bkc.DefaultRings)
	definitions.Components = []bkc.Entry{
		{ID: "com.gone", Versions: map[string]string{"dev": "1.0"}},
		{ID: "com.also.gone", Versions: map[string]string{"dev": "1.0"}},
		{ID: "com.test.nic", Versions: map[string]string{"dev": "1.0"}},
	}
	require.NoError(t, definitions.Write(file))
	before, err := os.ReadFile(file)
	require.NoError(t, err)

	_, err = promoteRing(t, file, metadata, "dev", func() { args.Rollout.Promote.Prune = true })
	assert.ErrorContains(t, err, "--prune would remove 2 of the 3 entries")
	after, err := os.ReadFile(file)
	require.NoError(t, err)
	assert.Equal(t, string(before), string(after), "a mirror that is not the right one must not empty the file")

	_, err = promoteRing(t, file, metadata, "dev", func() { args.Rollout.Promote.Prune = true; args.Rollout.Promote.DryRun = true })
	assert.NoError(t, err, "a preview shows it, it does not fail on it")

	_, err = promoteRing(t, file, metadata, "dev", func() { args.Rollout.Promote.Prune = true; args.Rollout.Promote.AllowMassPrune = true })
	require.NoError(t, err)
	loaded, err := bkc.Load(context.Background(), file, false)
	require.NoError(t, err)
	assert.Len(t, loaded.Components, 1)
}

func TestPromoteCreatesTheFileWhenItIsEmpty(t *testing.T) {
	withArgs(t)
	metadata := writeMetadata(t, "metadata.xml.zst", "1.0")
	file := filepath.Join(t.TempDir(), "bkc.yml")
	require.NoError(t, os.WriteFile(file, nil, 0o644))

	_, err := promoteRing(t, file, metadata, "dev")

	require.NoError(t, err)
	loaded, err := bkc.Load(context.Background(), file, false)
	require.NoError(t, err)
	assert.Len(t, loaded.Components, 1)
}

func TestPromoteReadsMetadataOverPlainHTTPOnlyWhenAllowedTo(t *testing.T) {
	withArgs(t)
	file := filepath.Join(t.TempDir(), "bkc.yml")

	_, err := promoteRing(t, file, "http://mirror.example.com/metadata.xml.zst", "dev")
	assert.ErrorContains(t, err, "plain HTTP")
	_, err = promoteRing(t, file, "HTTP://mirror.example.com/metadata.xml.zst", "dev")
	assert.ErrorContains(t, err, "plain HTTP", "the scheme is not case sensitive")
}

func TestPromoteFromStorageDoesNotCreateAMistypedOutputDirectory(t *testing.T) {
	withArgs(t)
	args.OutputDir = filepath.Join(t.TempDir(), "typo")

	_, err := promoteRing(t, filepath.Join(t.TempDir(), "bkc.yml"), "", "dev")

	assert.ErrorContains(t, err, "output directory")
	assert.NoDirExists(t, args.OutputDir)
}

func TestPromoteSaysNoCredentialsOfAURLItCannotEdit(t *testing.T) {
	withArgs(t)

	_, err := promoteRing(t, "https://user:token@host/bkc.yml?sig=abc", writeMetadata(t, "metadata.xml.zst", "1.0"), "dev")

	require.Error(t, err)
	assert.NotContains(t, err.Error(), "token")
	assert.NotContains(t, err.Error(), "abc")
}

func TestPromoteNeedsTheDirectoryOfTheFile(t *testing.T) {
	withArgs(t)

	_, err := promoteRing(t, filepath.Join(t.TempDir(), "nowhere", "bkc.yml"), writeMetadata(t, "metadata.xml.zst", "1.0"), "dev")

	assert.ErrorContains(t, err, "directory of --bkc.file")
}

func TestPromoteChecksTheDirectoryAFileLinkPointsInto(t *testing.T) {
	withArgs(t)
	link := filepath.Join(t.TempDir(), "bkc.yml")
	require.NoError(t, os.Symlink(filepath.Join(t.TempDir(), "gone", "bkc.yml"), link))

	_, err := promoteRing(t, link, writeMetadata(t, "metadata.xml.zst", "1.0"), "dev")

	assert.ErrorContains(t, err, "directory of --bkc.file")
}

func TestPromoteCreatesTheFileWhenItHoldsOnlyAComment(t *testing.T) {
	withArgs(t)
	file := filepath.Join(t.TempDir(), "bkc.yml")
	require.NoError(t, os.WriteFile(file, []byte("# rings, to fill in\n"), 0o644))

	_, err := promoteRing(t, file, writeMetadata(t, "metadata.xml.zst", "1.0"), "dev")

	require.NoError(t, err)
}

func TestPromoteSaysWhyItCannotReadTheFile(t *testing.T) {
	withArgs(t)
	dir := filepath.Join(t.TempDir(), "unreadable")
	require.NoError(t, os.Mkdir(dir, 0o755))

	_, err := promoteRing(t, dir, writeMetadata(t, "metadata.xml.zst", "1.0"), "dev")

	assert.ErrorContains(t, err, "reading the rollout rings", "a directory is not a file to create")
}

func TestPromoteReadsAFilePathAsAPathWhateverItLooksLike(t *testing.T) {
	withArgs(t)
	t.Chdir(t.TempDir())
	// What file:http%3A%2F%2Fx decodes to: a URL to look at, a file to open.
	require.NoError(t, os.Mkdir("http:", 0o755))
	require.NoError(t, bkc.New([]string{"dev"}).Write("http://x"))

	base, err := promotionBase("http://x")

	require.NoError(t, err)
	assert.Equal(t, []string{"dev"}, base.Rings)
}
