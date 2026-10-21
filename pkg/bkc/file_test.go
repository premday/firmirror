package bkc

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const sample = `version: 1
rings: [dev, beta]
components:
  - id: com.example.nic
    platform: 0123456789abcdef
    name: Example NIC
    versions: {dev: "2.90", beta: "2.9"}
`

func TestReadKeepsVersionsAsText(t *testing.T) {
	file, err := parse([]byte(sample), false)
	require.NoError(t, err)

	assert.Equal(t, DefaultTagPrefix, file.TagPrefix)
	assert.Equal(t, "2.90", file.Components[0].Versions["dev"], "2.90 is not the number 2.9")
	assert.Equal(t, "2.9", file.Components[0].Versions["beta"])
}

func TestValidate(t *testing.T) {
	tests := map[string]string{
		"unknown version":    "version: 2\nrings: [dev]\n",
		"no rings":           "version: 1\nrings: []\n",
		"uppercase ring":     "version: 1\nrings: [Dev]\n",
		"ring with a hyphen": "version: 1\nrings: [dev, stable-]\n",
		"duplicate ring":     "version: 1\nrings: [dev, dev]\n",
		"bad prefix":         "version: 1\ntag_prefix: Fir_mirror\nrings: [dev]\n",
		"component no id":    "version: 1\nrings: [dev]\ncomponents:\n  - versions: {dev: \"1\"}\n",
		"duplicate entry":    "version: 1\nrings: [dev]\ncomponents:\n  - {id: a, versions: {dev: \"1\"}}\n  - {id: a, versions: {dev: \"2\"}}\n",
		"undeclared ring":    "version: 1\nrings: [dev]\ncomponents:\n  - {id: a, versions: {stable: \"1\"}}\n",
		"empty version":      "version: 1\nrings: [dev]\ncomponents:\n  - {id: a, versions: {dev: \"\"}}\n",
		"unknown field":      "version: 1\nrings: [dev]\nholds: {a: \"1\"}\n",
		"fractional version": "version: 1.5\nrings: [dev]\n",
		"float version":      "version: 1.0\nrings: [dev]\n",
		"quoted version":     "version: \"1\"\nrings: [dev]\n",
	}
	for name, document := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := parse([]byte(document), false)
			assert.Error(t, err)
		})
	}
}

func TestMarshalIsStableAndRoundTrips(t *testing.T) {
	file := New(DefaultRings)
	file.Components = []Entry{
		{ID: "b", Versions: map[string]string{"stable": "1.0", "dev": "2.0"}},
		{ID: "a", Platform: Platform([]string{"g2", "g1"}), Name: "A", Versions: map[string]string{"dev": "2.90"}},
	}

	data, err := file.Marshal()
	require.NoError(t, err)

	assert.Equal(t, fmt.Sprintf(`version: 1
rings: [dev, beta, preview, stable]
components:
  - id: a
    platform: %s
    name: A
    versions: {dev: "2.90"}
  - id: b
    versions: {dev: "2.0", stable: "1.0"}
`, Platform([]string{"g1", "g2"})), string(data), "sorted by key, versions in ring order")

	again, err := parse(data, false)
	require.NoError(t, err)
	second, err := again.Marshal()
	require.NoError(t, err)
	assert.Equal(t, string(data), string(second))
}

func TestLoadFromAPathAndAURL(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "bkc.yml")
	require.NoError(t, os.WriteFile(path, []byte(sample), 0o644))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/firmirror_bkc" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(sample))
	}))
	defer server.Close()

	fromPath, err := Load(ctx, path, false)
	require.NoError(t, err)
	fromURL, err := Load(ctx, server.URL+"/api/v1/firmirror_bkc", true)
	require.NoError(t, err)
	assert.Equal(t, fromPath, fromURL)

	_, err = Load(ctx, server.URL+"/missing", true)
	assert.ErrorContains(t, err, "404")
	_, err = Load(ctx, filepath.Join(t.TempDir(), "nowhere.yml"), false)
	assert.Error(t, err)
}

func TestWriteReplacesTheFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bkc.yml")
	require.NoError(t, os.WriteFile(path, []byte("stale"), 0o600))

	require.NoError(t, New([]string{"dev"}).Write(path))

	loaded, err := Load(context.Background(), path, false)
	require.NoError(t, err)
	assert.Equal(t, []string{"dev"}, loaded.Rings)
}

func TestMarshalQuotesWhatWouldBeReadAsSomethingElse(t *testing.T) {
	file := New([]string{"null", "yes"})
	file.Components = []Entry{{ID: "true", Platform: "12345678", Name: "null", Versions: map[string]string{"null": "1", "yes": "2"}}}

	data, err := file.Marshal()
	require.NoError(t, err)
	again, err := parse(data, false)

	require.NoError(t, err, string(data))
	assert.Equal(t, file, again)
}

func TestMarshalQuotesWhatYAML11ReadsAsABoolean(t *testing.T) {
	file := New([]string{"dev", "on"})
	file.Components = []Entry{{ID: "yes", Name: "No", Versions: map[string]string{"dev": "1"}}}

	data, err := file.Marshal()
	require.NoError(t, err)

	assert.Contains(t, string(data), `rings: [dev, "on"]`)
	assert.Contains(t, string(data), `id: "yes"`)
	assert.Contains(t, string(data), `name: "No"`)
}

func TestParseTellsASecondDocumentFromABrokenOne(t *testing.T) {
	_, err := parse([]byte(sample+"---\nnull\n"), false)
	assert.ErrorContains(t, err, "more than one document")

	_, err = parse([]byte(sample+"---\nrings: [dev\n"), false)
	assert.ErrorContains(t, err, "after the first document")

	_, err = parse([]byte(sample+"---\n"), false)
	assert.NoError(t, err, "a trailing marker is an empty document")
}

func TestParseJudgesTheVersionBeforeTheFields(t *testing.T) {
	_, err := parse([]byte("version: 2\nrings: [dev]\ncomponents:\n  - id: a\n    channels: [x]\n    versions: {dev: \"1\"}\n"), false)

	assert.ErrorContains(t, err, "version 2 is not one this firmirror knows")
}

func TestLoadIgnoresFieldsOfAnAheadServiceButNotOfAFile(t *testing.T) {
	ctx := context.Background()
	ahead := sample + "generated_at: 2026-01-01\n"
	path := filepath.Join(t.TempDir(), "bkc.yml")
	require.NoError(t, os.WriteFile(path, []byte(ahead), 0o644))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(ahead + "extra: [1]\n"))
	}))
	defer server.Close()

	_, err := Load(ctx, path, false)
	assert.ErrorContains(t, err, "generated_at", "in a file it is a typo")
	file, err := Load(ctx, server.URL, true)
	require.NoError(t, err)
	assert.Equal(t, "com.example.nic", file.Components[0].ID)

	broken := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("version: 2\nrings: [dev]\nextra: 1\n"))
	}))
	defer broken.Close()
	_, err = Load(ctx, broken.URL, true)
	assert.ErrorContains(t, err, "version 2", "the version is what gates a layout this firmirror cannot read")
}

func TestWriteFollowsASymlinkAndKeepsTheMode(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target.yml")
	link := filepath.Join(dir, "bkc.yml")
	require.NoError(t, os.WriteFile(target, []byte("stale"), 0o600))
	require.NoError(t, os.Symlink(target, link))

	require.NoError(t, New([]string{"dev"}).Write(link))

	info, err := os.Lstat(link)
	require.NoError(t, err)
	assert.NotZero(t, info.Mode()&os.ModeSymlink, "the link is still a link")
	loaded, err := Load(context.Background(), target, false)
	require.NoError(t, err)
	assert.Equal(t, []string{"dev"}, loaded.Rings)
	info, err = os.Stat(target)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
}

func TestLoadIgnoresOnlyUnknownFieldsAtTheTopOfARemoteFile(t *testing.T) {
	typo := "version: 1\nrings: [dev]\ncomponents:\n  - {id: a, versoins: {dev: \"1\"}}\n"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(typo)) }))
	defer server.Close()

	_, err := Load(context.Background(), server.URL, true)

	assert.ErrorContains(t, err, "versoins", "a component with no versions would be served by no ring, and nothing would say so")
}

func TestLoadRefusesAFileThatIsTooBig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bkc.yml")
	require.NoError(t, os.WriteFile(path, []byte(sample+"# "+strings.Repeat("x", MaxFileSize)), 0o644))

	_, err := Load(context.Background(), path, false)

	assert.ErrorContains(t, err, "bytes")
}

func TestLoadRefusesPlainHTTPUnlessAllowedTo(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(sample)) }))
	defer server.Close()

	_, err := Load(context.Background(), server.URL, false)
	assert.ErrorContains(t, err, "plain HTTP")
	_, err = Load(context.Background(), strings.ToUpper(server.URL[:4])+server.URL[4:], false)
	assert.ErrorContains(t, err, "plain HTTP", "the scheme is not case sensitive")
	_, err = Load(context.Background(), server.URL, true)
	assert.NoError(t, err)
}

func TestLoadDoesNotSayTheCredentialsOfAURL(t *testing.T) {
	_, err := Load(context.Background(), "http://user:token@127.0.0.1:1/bkc.yml", false)

	require.Error(t, err)
	assert.NotContains(t, err.Error(), "token")
	assert.NotContains(t, err.Error(), "user")
}

func TestLoadKeepsWhatAnAliasOfAnUnknownFieldMeans(t *testing.T) {
	ahead := "version: 1\nrings: [dev]\nx-defaults: &d {dev: \"1\"}\nfuture: 1\ncomponents:\n  - {id: a, versions: *d}\n"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(ahead)) }))
	defer server.Close()

	file, err := Load(context.Background(), server.URL, true)

	require.NoError(t, err)
	assert.Equal(t, "1", file.Components[0].Versions["dev"])
}

func TestParseRefusesWhatTheDecoderWouldDropSilently(t *testing.T) {
	tests := map[string]string{
		"empty ring":      "version: 1\nrings:\n  - dev\n  -\n  - beta\n",
		"empty component": "version: 1\nrings: [dev]\ncomponents:\n  -\n  - {id: a, versions: {dev: \"1\"}}\n",
		"second document": "version: 1\nrings: [dev]\n---\nversion: 1\nrings: [beta]\n",
	}
	for name, document := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := parse([]byte(document), false)
			assert.Error(t, err)
		})
	}
}

func TestValidateSaysTheSameRingEveryTime(t *testing.T) {
	file := New([]string{"dev"})
	file.Components = []Entry{{ID: "a", Versions: map[string]string{"z": "1", "y": "1", "x": "1"}}}

	for i := 0; i < 20; i++ {
		assert.ErrorContains(t, file.Validate(), `ring "x"`)
	}
}

func TestParseToleratesATrailingEmptyDocument(t *testing.T) {
	_, err := parse([]byte(sample+"---\n"), false)

	assert.NoError(t, err)
}

func TestFollowLinksFailsOnAChainThatDoesNotEnd(t *testing.T) {
	dir := t.TempDir()
	one, two := filepath.Join(dir, "one"), filepath.Join(dir, "two")
	require.NoError(t, os.Symlink(two, one))
	require.NoError(t, os.Symlink(one, two))

	_, err := FollowLinks(one)

	assert.ErrorContains(t, err, "too many levels")
	assert.Error(t, New([]string{"dev"}).Write(one), "the link is not replaced by a regular file")
}

func TestFollowLinksResolvesARelativeTargetFromTheRealDirectory(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "infra", "firmirror"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(root, "infra", "rings.yml"), []byte(sample), 0o644))
	require.NoError(t, os.MkdirAll(filepath.Join(root, "etc"), 0o755))
	require.NoError(t, os.Symlink(filepath.Join(root, "infra", "firmirror"), filepath.Join(root, "etc", "firmirror")))
	require.NoError(t, os.Symlink("../rings.yml", filepath.Join(root, "infra", "firmirror", "bkc.yml")))
	viaLink := filepath.Join(root, "etc", "firmirror", "bkc.yml")

	resolved, err := FollowLinks(viaLink)
	require.NoError(t, err)
	expected, err := filepath.EvalSymlinks(filepath.Join(root, "infra", "rings.yml"))
	require.NoError(t, err)
	assert.Equal(t, expected, resolved, ".. leaves the directory the link is in, not the one it was reached through")

	file, err := Load(context.Background(), viaLink, false)
	require.NoError(t, err)
	require.NoError(t, file.Write(viaLink))
	assert.NoFileExists(t, filepath.Join(root, "etc", "rings.yml"))
}

func TestWriteIsReadBackWhateverTheTextOfAnEntry(t *testing.T) {
	file := New([]string{"dev"})
	file.Components = []Entry{{ID: "a", Name: "\tx\ny", Versions: map[string]string{"dev": "1"}}}
	path := filepath.Join(t.TempDir(), "bkc.yml")

	require.NoError(t, file.Write(path))

	loaded, err := Load(context.Background(), path, false)
	require.NoError(t, err)
	assert.Equal(t, "\tx\ny", loaded.Components[0].Name)
}

func TestWriteRefusesWhatItCouldNotReadBack(t *testing.T) {
	file := New([]string{"dev"})
	file.Version = 2
	path := filepath.Join(t.TempDir(), "bkc.yml")

	assert.ErrorContains(t, file.Write(path), "would not be read back")
	assert.NoFileExists(t, path)
}

func TestBlank(t *testing.T) {
	assert.True(t, Blank(nil))
	assert.True(t, Blank([]byte("\n")))
	assert.True(t, Blank([]byte("# rings, to fill in\n")))
	assert.True(t, Blank([]byte("---\n")))
	assert.True(t, Blank([]byte("---\n# rings, to fill in\n")))
	assert.False(t, Blank([]byte("null\n")))
	assert.False(t, Blank([]byte("---\n---\n"+sample)), "a file that starts with an empty document is not empty")
	assert.False(t, Blank([]byte(sample)))
}
