package firmirror

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/premday/firmirror/pkg/lvfs"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// quickJCATRetries makes the retries of reading the stored JCAT instant for a test
// that makes it fail.
func quickJCATRetries(t *testing.T) {
	t.Helper()
	saved := jcatRetryDelay
	jcatRetryDelay = time.Millisecond
	t.Cleanup(func() { jcatRetryDelay = saved })
}

const signingCertificate, signingKey = "/secrets/signing.cert", "/secrets/signing.key"

// publishSigned publishes a document with signing keys, which leaves a signed JCAT.
func publishSigned(t *testing.T, dir string) {
	t.Helper()
	signed := newBaseURLSyncer(t, dir, "")
	signed.Config.Certificate, signed.Config.PrivateKey = signingCertificate, signingKey
	signed.newComponents = []lvfs.Component{baseURLTestComponent()}
	require.NoError(t, signed.SaveMetadata(context.Background()))
}

// unsignedRun is a syncer without signing keys that has loaded what dir holds and
// mirrored a component the document has not got.
func unsignedRun(t *testing.T, dir string) *FirmirrorSyncer {
	t.Helper()
	syncer := newBaseURLSyncer(t, dir, "")
	require.NoError(t, syncer.LoadMetadata(context.Background()))
	other := baseURLTestComponent()
	other.ID = "com.test.firmware2"
	syncer.newComponents = []lvfs.Component{other}
	return syncer
}

func jcatPath(dir string) string { return filepath.Join(dir, "metadata.xml.zst.jcat") }

func TestAPublishWithoutKeysDoesNotReplaceASignedJCAT(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	publishSigned(t, dir)
	syncer := unsignedRun(t, dir)

	assert.ErrorContains(t, syncer.SaveMetadata(ctx), "stored JCAT is signed")

	jcat, err := syncer.readStoredJCAT(ctx)
	require.NoError(t, err)
	assert.True(t, jcat.isSigned(), "what clients verify is left as it was")
}

func TestStopSigningStopsSigning(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	publishSigned(t, dir)
	syncer := unsignedRun(t, dir)
	syncer.Config.StopSigning = true

	require.NoError(t, syncer.SaveMetadata(ctx))

	jcat, err := syncer.readStoredJCAT(ctx)
	require.NoError(t, err)
	assert.False(t, jcat.isSigned())
}

func TestAJCATThatIsCertainlyNotOneIsReplacedByAnUnsignedRun(t *testing.T) {
	for name, content := range map[string][]byte{
		"empty":        nil,
		"not gzip":     []byte("not a jcat"),
		"not JSON":     gzipped(t, "not JSON"),
		"wrong header": {0x1f, 0x8b, 0x09, 0, 0, 0, 0, 0, 0, 0},
	} {
		t.Run(name, func(t *testing.T) {
			quickJCATRetries(t)
			dir := t.TempDir()
			publishSigned(t, dir)
			require.NoError(t, os.WriteFile(jcatPath(dir), content, 0o644))
			syncer := unsignedRun(t, dir)

			assert.NoError(t, syncer.Preflight(context.Background()))
			assert.NoError(t, syncer.SaveMetadata(context.Background()), "what is not a JCAT is no signature to protect")
		})
	}
}

func TestAJCATThatIsCutShortIsNotKnownToBeUnsigned(t *testing.T) {
	quickJCATRetries(t)
	ctx := context.Background()
	dir := t.TempDir()
	publishSigned(t, dir)
	require.NoError(t, os.WriteFile(jcatPath(dir), []byte{0x1f, 0x8b, 0x08}, 0o644))
	syncer := unsignedRun(t, dir)

	assert.ErrorContains(t, syncer.Preflight(ctx), "delete it if it is cut short", "it may be signed, and says what to do")
	assert.Error(t, syncer.SaveMetadata(ctx))
	syncer.Config.StopSigning = true
	assert.NoError(t, syncer.Preflight(ctx), "stopping signing on purpose is what StopSigning is for")
	assert.NoError(t, syncer.SaveMetadata(ctx))
}

func TestAJCATOfAnotherShapeIsNotKnownToBeUnsigned(t *testing.T) {
	quickJCATRetries(t)
	dir := t.TempDir()
	publishSigned(t, dir)
	require.NoError(t, os.WriteFile(jcatPath(dir), gzipped(t, `{"Items": "a newer libjcat wrote this"}`), 0o644))
	syncer := unsignedRun(t, dir)

	assert.Error(t, syncer.Preflight(context.Background()), "it may be signed, and is not replaced")
}

func TestAJCATOfAnUnknownLayoutIsNotKnownToBeUnsigned(t *testing.T) {
	for name, content := range map[string]string{
		"empty object":       `{}`,
		"null":               `null`,
		"items renamed":      `{"Entries": []}`,
		"item without blobs": `{"Items": [{}]}`,
		"blob without kind":  `{"Items": [{"Blobs": [{"Data": "x"}]}]}`,
		"blob without data":  `{"Items": [{"Blobs": [{"Kind": 3}]}]}`,
		"newer major":        `{"JcatVersionMajor": 1, "Items": []}`,
	} {
		t.Run(name, func(t *testing.T) {
			quickJCATRetries(t)
			dir := t.TempDir()
			publishSigned(t, dir)
			require.NoError(t, os.WriteFile(jcatPath(dir), gzipped(t, content), 0o644))
			syncer := unsignedRun(t, dir)

			err := syncer.Preflight(context.Background())

			assert.ErrorContains(t, err, "could not check whether the stored JCAT is signed")
			assert.NotErrorIs(t, err, errMalformedJCAT)
		})
	}
}

func TestBytesAfterTheGzipStreamOfASignedJCATAreIgnored(t *testing.T) {
	quickJCATRetries(t)
	dir := t.TempDir()
	publishSigned(t, dir)
	stored, err := os.ReadFile(jcatPath(dir))
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(jcatPath(dir), append(stored, make([]byte, 16)...), 0o644))
	syncer := unsignedRun(t, dir)

	assert.ErrorContains(t, syncer.Preflight(context.Background()), "the stored JCAT is signed")
}

func TestBytesAfterASignedJCATDoNotMakeItMalformed(t *testing.T) {
	quickJCATRetries(t)
	dir := t.TempDir()
	publishSigned(t, dir)
	stored, err := os.Open(jcatPath(dir))
	require.NoError(t, err)
	defer stored.Close()
	unzipper, err := gzip.NewReader(stored)
	require.NoError(t, err)
	content, err := io.ReadAll(unzipper)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(jcatPath(dir), gzipped(t, string(content)+"\ntrailing"), 0o644))
	syncer := unsignedRun(t, dir)

	assert.ErrorContains(t, syncer.Preflight(context.Background()), "signed", "it still holds a signature, which is not replaced")
}

func TestStopSigningReplacesAJCATThatCannotBeReadEvenForAnUnchangedDocument(t *testing.T) {
	quickJCATRetries(t)
	ctx := context.Background()
	dir := t.TempDir()
	publishSigned(t, dir)
	require.NoError(t, os.WriteFile(jcatPath(dir), []byte{0x1f, 0x8b, 0x08}, 0o644))
	syncer := newBaseURLSyncer(t, dir, "")
	syncer.Config.StopSigning = true
	require.NoError(t, syncer.LoadMetadata(ctx))

	require.NoError(t, syncer.SaveMetadata(ctx))

	jcat, err := syncer.readStoredJCAT(ctx)
	require.NoError(t, err, "the unreadable JCAT was asked to be replaced")
	assert.True(t, jcat.hasChecksum(fileChecksumOfStored(t, dir)))
}

func TestPreflightRefusesToUnsignAJCAT(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	publishSigned(t, dir)

	unsigned := newBaseURLSyncer(t, dir, "")
	require.NoError(t, unsigned.LoadMetadata(ctx))
	assert.ErrorContains(t, unsigned.Preflight(ctx), "stored JCAT is signed")

	unsigned.Config.Certificate, unsigned.Config.PrivateKey = signingCertificate, signingKey
	assert.NoError(t, unsigned.Preflight(ctx))
}

func TestASignedJCATWithNoMetadataIsNotWorthProtecting(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	publishSigned(t, dir)
	require.NoError(t, os.Remove(filepath.Join(dir, "metadata.xml.zst")))
	syncer := unsignedRun(t, dir)

	assert.NoError(t, syncer.Preflight(ctx))
	assert.NoError(t, syncer.SaveMetadata(ctx), "there is no metadata for the signature to protect")
}

func TestASignedRunRepairsAJCATThatIsCutShort(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	publishSigned(t, dir)
	require.NoError(t, os.WriteFile(jcatPath(dir), []byte{0x1f, 0x8b, 0x08}, 0o644))
	syncer := newBaseURLSyncer(t, dir, "")
	syncer.Config.Certificate, syncer.Config.PrivateKey = signingCertificate, signingKey
	require.NoError(t, syncer.LoadMetadata(ctx))

	require.NoError(t, syncer.SaveMetadata(ctx), "with the keys, there is a signature to put back")

	jcat, err := syncer.readStoredJCAT(ctx)
	require.NoError(t, err)
	assert.True(t, jcat.isSigned())
}

// failingJCAT is a storage whose JCAT cannot be found out about, as a storage
// error would.
type failingJCAT struct{ Storage }

func (f failingJCAT) Exists(ctx context.Context, key string) (bool, error) {
	if key == jcatKey {
		return false, errors.New("storage unavailable")
	}
	return f.Storage.Exists(ctx, key)
}

func TestAJCATThatCouldNotBeReadIsNotReplaced(t *testing.T) {
	quickJCATRetries(t)
	ctx := context.Background()
	dir := t.TempDir()
	publishSigned(t, dir)
	syncer := unsignedRun(t, dir)
	syncer.Storage = failingJCAT{syncer.Storage}

	assert.ErrorContains(t, syncer.SaveMetadata(ctx), "could not check whether the stored JCAT is signed")
}

// resetJCAT is a storage whose JCAT can be found but not read to the end.
type resetJCAT struct{ Storage }

type resettingReader struct{}

func (resettingReader) Read([]byte) (int, error) { return 0, errors.New("connection reset by peer") }
func (resettingReader) Close() error             { return nil }

func (r resetJCAT) Read(ctx context.Context, key string) (io.ReadCloser, error) {
	if key == jcatKey {
		return resettingReader{}, nil
	}
	return r.Storage.Read(ctx, key)
}

func TestAConnectionResetIsNotAMalformedJCAT(t *testing.T) {
	quickJCATRetries(t)
	dir := t.TempDir()
	publishSigned(t, dir)
	syncer := newBaseURLSyncer(t, dir, "")
	syncer.Storage = resetJCAT{syncer.Storage}

	_, err := syncer.readStoredJCAT(context.Background())

	require.Error(t, err)
	assert.NotErrorIs(t, err, errMalformedJCAT, "what could not be read may be signed, and is not replaced")
}

// flakyJCAT is a storage whose JCAT cannot be found out about the first times.
type flakyJCAT struct {
	Storage
	failures int
}

func (f *flakyJCAT) Exists(ctx context.Context, key string) (bool, error) {
	if key == jcatKey && f.failures > 0 {
		f.failures--
		return false, errors.New("storage unavailable")
	}
	return f.Storage.Exists(ctx, key)
}

func TestAStorageThatFailsOnceDoesNotLoseTheRun(t *testing.T) {
	quickJCATRetries(t)
	dir := t.TempDir()
	publishSigned(t, dir)
	syncer := unsignedRun(t, dir)
	syncer.Config.StopSigning = true
	syncer.Storage = &flakyJCAT{Storage: syncer.Storage, failures: 1}

	assert.NoError(t, syncer.SaveMetadata(context.Background()))
}

func TestTheStoredJCATIsReadAgainTwiceBeforeGivingUp(t *testing.T) {
	for failures, want := range map[int]string{
		2: "the stored JCAT is signed",
		3: "could not check whether the stored JCAT is signed",
	} {
		t.Run(fmt.Sprint(failures, " failures"), func(t *testing.T) {
			quickJCATRetries(t)
			dir := t.TempDir()
			publishSigned(t, dir)
			syncer := unsignedRun(t, dir)
			syncer.Storage = &flakyJCAT{Storage: syncer.Storage, failures: failures}

			assert.ErrorContains(t, syncer.Preflight(context.Background()), want)
		})
	}
}

func gzipped(t *testing.T, content string) []byte {
	t.Helper()
	var compressed bytes.Buffer
	zipper := gzip.NewWriter(&compressed)
	_, err := zipper.Write([]byte(content))
	require.NoError(t, err)
	require.NoError(t, zipper.Close())
	return compressed.Bytes()
}
