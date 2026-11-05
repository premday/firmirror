package daemon

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/premday/firmirror/pkg/bkc"
)

const rings = `version: 1
rings: [dev, beta]
components:
  - id: com.example.nic
    platform: 0123456789abcdef
    versions: {dev: "2.0", beta: "1.0"}
`

// edited and fixed are rings that serve other versions than rings does.
var (
	edited = strings.Replace(rings, `"2.0"`, `"3.0"`, 1)
	fixed  = strings.Replace(rings, `"2.0"`, `"4.0"`, 1)
)

func write(t *testing.T, path, content string) {
	t.Helper()
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))
}

// watchFrom starts a watcher on source that has seen the content it holds now.
func watchFrom(t *testing.T, source string, poll time.Duration) (*Watcher, chan struct{}) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	_, seen, err := bkc.LoadWithDigest(ctx, source, true)
	require.NoError(t, err)
	w := &Watcher{Source: source, AllowHTTP: true, PollInterval: poll, Debounce: 20 * time.Millisecond, Seen: seen, Metrics: NewMetrics()}
	updates := make(chan struct{}, 1)
	go w.Run(ctx, updates)
	// The watch is set up before the first read, which the metrics tell.
	require.Eventually(t, func() bool { return testutil.ToFloat64(w.Metrics.bkcLastCheck) > 0 }, 5*time.Second, 5*time.Millisecond)
	return w, updates
}

func expectUpdate(t *testing.T, updates chan struct{}) {
	t.Helper()
	select {
	case <-updates:
	case <-time.After(5 * time.Second):
		t.Fatal("expected an update")
	}
}

func expectNone(t *testing.T, updates chan struct{}) {
	t.Helper()
	select {
	case <-updates:
		t.Fatal("unexpected update")
	case <-time.After(300 * time.Millisecond):
	}
}

func TestWatcherSeesAFileBeingWrittenInPlace(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rings.yml")
	write(t, path, rings)
	w, updates := watchFrom(t, path, time.Hour)

	write(t, path, edited)
	expectUpdate(t, updates)

	expectNone(t, updates)
	assert.Equal(t, 1.0, testutil.ToFloat64(w.Metrics.bkcChecks.WithLabelValues(resultChanged)))
}

func TestWatcherSeesAFileReplacedByARename(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "rings.yml")
	write(t, path, rings)
	_, updates := watchFrom(t, path, time.Hour)

	write(t, filepath.Join(dir, ".rings.yml.tmp"), edited)
	require.NoError(t, os.Rename(filepath.Join(dir, ".rings.yml.tmp"), path))
	expectUpdate(t, updates)
}

func TestWatcherSeesASymlinkSwap(t *testing.T) {
	// A volume made of a ConfigMap: rings.yml -> ..data/rings.yml, and ..data
	// is swapped for another directory.
	dir := t.TempDir()
	for _, name := range []string{"v1", "v2"} {
		require.NoError(t, os.Mkdir(filepath.Join(dir, name), 0o755))
	}
	write(t, filepath.Join(dir, "v1", "rings.yml"), rings)
	write(t, filepath.Join(dir, "v2", "rings.yml"), edited)
	require.NoError(t, os.Symlink("v1", filepath.Join(dir, "..data")))
	require.NoError(t, os.Symlink("..data/rings.yml", filepath.Join(dir, "rings.yml")))
	_, updates := watchFrom(t, filepath.Join(dir, "rings.yml"), time.Hour)

	require.NoError(t, os.Symlink("v2", filepath.Join(dir, "..data_tmp")))
	require.NoError(t, os.Rename(filepath.Join(dir, "..data_tmp"), filepath.Join(dir, "..data")))
	expectUpdate(t, updates)
}

func TestWatcherIgnoresEventsThatChangeNothing(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "rings.yml")
	write(t, path, rings)
	w, updates := watchFrom(t, path, time.Hour)

	write(t, path, rings) // same bytes
	write(t, filepath.Join(dir, "other"), "unrelated")
	expectNone(t, updates)
	assert.Equal(t, 0.0, testutil.ToFloat64(w.Metrics.bkcChecks.WithLabelValues(resultChanged)))
}

func TestWatcherIgnoresAnEditThatChangesNothingTheRingsRender(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rings.yml")
	write(t, path, rings)
	w, updates := watchFrom(t, path, time.Hour)

	write(t, path, "# a comment\n"+rings)
	expectNone(t, updates)
	assert.Equal(t, 0.0, testutil.ToFloat64(w.Metrics.bkcChecks.WithLabelValues(resultChanged)))

	write(t, path, "# a comment\n"+edited)
	expectUpdate(t, updates)
}

func TestWatcherIgnoresAChangeOfNamesAndOfOtherFilesOfTheDirectory(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "rings.yml")
	write(t, path, rings)
	w, updates := watchFrom(t, path, time.Hour)
	checks := func() float64 { return testutil.ToFloat64(w.Metrics.bkcChecks.WithLabelValues(resultUnchanged)) }
	before := checks()

	write(t, filepath.Join(dir, "other"), "unrelated")
	expectNone(t, updates)
	assert.Equal(t, before, checks(), "the rings file was not even read for a file next to it")

	write(t, path, strings.Replace(rings, "platform: 0123456789abcdef", "platform: 0123456789abcdef\n    name: Only a name", 1))
	expectNone(t, updates)
	assert.Equal(t, before+1, checks(), "read, and found to serve the same")
}

func TestWatcherFollowsADirectoryLinkSwappedForAnother(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"v1", "v2"} {
		require.NoError(t, os.MkdirAll(filepath.Join(dir, "releases", name), 0o755))
		write(t, filepath.Join(dir, "releases", name, "rings.yml"), rings)
	}
	require.NoError(t, os.Symlink(filepath.Join(dir, "releases", "v1"), filepath.Join(dir, "current")))
	w, updates := watchFrom(t, filepath.Join(dir, "current", "rings.yml"), 100*time.Millisecond)

	// The link is swapped, and the poll sees it and watches the new directory.
	require.NoError(t, os.Symlink(filepath.Join(dir, "releases", "v2"), filepath.Join(dir, "next")))
	require.NoError(t, os.Rename(filepath.Join(dir, "next"), filepath.Join(dir, "current")))
	write(t, filepath.Join(dir, "releases", "v2", "rings.yml"), edited)
	expectUpdate(t, updates)
	require.Greater(t, testutil.ToFloat64(w.Metrics.bkcChecks.WithLabelValues(resultChanged)), 0.0)

	// A later edit in the new directory is seen too.
	write(t, filepath.Join(dir, "releases", "v2", "rings.yml"), fixed)
	expectUpdate(t, updates)
}

func TestWatcherKeepsTheRingsInUseWhenTheNewOnesAreInvalid(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rings.yml")
	write(t, path, rings)
	w, updates := watchFrom(t, path, time.Hour)

	write(t, path, "rings: [")
	expectNone(t, updates)
	assert.GreaterOrEqual(t, testutil.ToFloat64(w.Metrics.bkcChecks.WithLabelValues(resultError)), 1.0)

	write(t, path, fixed)
	expectUpdate(t, updates)
}

func TestWatcherReadsTheRingsAtStartForTheMetrics(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rings.yml")
	write(t, path, rings)
	w, _ := watchFrom(t, path, time.Hour)

	assert.Greater(t, testutil.ToFloat64(w.Metrics.bkcLastCheck), 0.0)
	assert.Equal(t, 1.0, testutil.ToFloat64(w.Metrics.bkcChecks.WithLabelValues(resultUnchanged)))
}

func TestWatcherSeesAChangeOfTheTargetOfALinkInAnotherDirectory(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(dir, "repo"), 0o755))
	require.NoError(t, os.Mkdir(filepath.Join(dir, "etc"), 0o755))
	target := filepath.Join(dir, "repo", "rings.yml")
	write(t, target, rings)
	require.NoError(t, os.Symlink(target, filepath.Join(dir, "etc", "rings.yml")))
	_, updates := watchFrom(t, filepath.Join(dir, "etc", "rings.yml"), time.Hour)

	write(t, target, edited)
	expectUpdate(t, updates)
}

func TestWatcherSeesAChangeWhileAnotherFileOfTheDirectoryIsWrittenAllTheTime(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "rings.yml")
	write(t, path, rings)
	_, updates := watchFrom(t, path, time.Hour)
	stop, exited := make(chan struct{}), make(chan struct{})
	defer func() {
		close(stop)
		<-exited
	}()
	go func() {
		defer close(exited)
		for {
			select {
			case <-stop:
				return
			case <-time.After(5 * time.Millisecond):
				_ = os.WriteFile(filepath.Join(dir, "noise"), []byte(time.Now().String()), 0o644)
			}
		}
	}()

	write(t, path, edited)

	expectUpdate(t, updates)
}

func TestWatcherPollsAURL(t *testing.T) {
	var version atomic.Value
	version.Store(rings)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(version.Load().(string)))
	}))
	defer server.Close()
	_, updates := watchFrom(t, server.URL, 30*time.Millisecond)

	expectNone(t, updates)
	version.Store(edited)
	expectUpdate(t, updates)
}
