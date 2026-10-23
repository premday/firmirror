package firmirror

import (
	"bytes"
	"compress/flate"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"time"

	"github.com/premday/firmirror/pkg/utils"
)

// jcatKindSHA256 is libjcat's JcatBlobKind for a SHA-256 checksum.
const jcatKindSHA256 = 1

// jcatSignatureKinds are libjcat's blob kinds for GPG, PKCS#7 and ED25519
// signatures.
var jcatSignatureKinds = []int{2, 3, 9}

// storedJCAT is the part of a JCAT firmirror reads. libjcat writes gzipped JSON:
// one item per file, holding blobs of a kind (checksum, signature) and their
// data, which for a checksum is its hex digest. The members are pointers to tell
// an absent one from an empty one.
type storedJCAT struct {
	VersionMajor *int `json:"JcatVersionMajor"`
	Items        *[]struct {
		Blobs *[]struct {
			Kind *int
			Data *string
		}
	}
}

// check fails on JSON that is not a JCAT of the version and shape libjcat writes,
// whatever Go decoded it to: an unknown layout may hold a signature.
func (j *storedJCAT) check() error {
	if j.VersionMajor != nil && *j.VersionMajor != 0 {
		return fmt.Errorf("JCAT version %d is not one this firmirror reads", *j.VersionMajor)
	}
	if j.Items == nil {
		return errors.New("JCAT has no items")
	}
	for _, item := range *j.Items {
		if item.Blobs == nil {
			return errors.New("JCAT item has no blobs")
		}
		for _, blob := range *item.Blobs {
			if blob.Kind == nil || blob.Data == nil {
				return errors.New("JCAT blob has no kind or data")
			}
		}
	}
	return nil
}

// errMalformedJCAT marks a stored JCAT that is certainly not one, so it holds no
// signature clients rely on, unlike one that could not be read.
var errMalformedJCAT = errors.New("malformed JCAT")

// jcatReadError wraps a read failure, as errMalformedJCAT only when what was read
// is certainly not a JCAT (empty, bad gzip, bad JSON, too large). A reset
// connection or a body cut short looks the same as a truncated file and may still
// be signed; so does well-formed JSON of another shape, which a newer libjcat may
// write.
func jcatReadError(what string, err error) error {
	var syntax *json.SyntaxError
	var corrupt flate.CorruptInputError
	if errors.Is(err, io.EOF) || errors.Is(err, utils.ErrTooLarge) || errors.Is(err, gzip.ErrHeader) || errors.Is(err, gzip.ErrChecksum) ||
		errors.As(err, &syntax) || errors.As(err, &corrupt) {
		return fmt.Errorf("%s: %w: %w", what, errMalformedJCAT, err)
	}
	return fmt.Errorf("%s: %w", what, err)
}

// maxJCATSize caps a stored JCAT, decompressed: it holds a few checksums and a
// signature.
const maxJCATSize = 64 << 20

// readStoredJCAT reads the JCAT stored beside the metadata. It returns nil when
// none is stored.
func (f *FirmirrorSyncer) readStoredJCAT(ctx context.Context) (*storedJCAT, error) {
	exists, err := f.Storage.Exists(ctx, jcatKey)
	if err != nil {
		return nil, fmt.Errorf("checking the stored JCAT: %w", err)
	}
	if !exists {
		return nil, nil
	}
	reader, err := f.Storage.Read(ctx, jcatKey)
	if err != nil {
		return nil, fmt.Errorf("reading the stored JCAT: %w", err)
	}
	defer reader.Close()
	uncompressed, err := gzip.NewReader(reader)
	if err != nil {
		return nil, jcatReadError("reading the stored JCAT, which is not a compressed JCAT", err)
	}
	defer uncompressed.Close()
	// Bytes after the stream are not another one to parse.
	uncompressed.Multistream(false)

	data, err := utils.ReadAllLimited(uncompressed, maxJCATSize, "the stored JCAT")
	if err != nil {
		return nil, jcatReadError("reading the stored JCAT", err)
	}
	var jcat storedJCAT
	// Decode only the first value, so trailing bytes do not hide a signature.
	if err := json.NewDecoder(bytes.NewReader(data)).Decode(&jcat); err != nil {
		if errors.Is(err, io.ErrUnexpectedEOF) {
			// The whole file was read, so the JSON is truncated.
			return nil, fmt.Errorf("reading the stored JCAT: %w: %w", errMalformedJCAT, err)
		}
		return nil, jcatReadError("reading the stored JCAT", err)
	}
	if err := jcat.check(); err != nil {
		return nil, fmt.Errorf("reading the stored JCAT: %w", err)
	}
	return &jcat, nil
}

// jcatRetryDelay is the wait before the first retry, doubled for the next.
var jcatRetryDelay = 250 * time.Millisecond

// readStoredJCATWithRetry reads the stored JCAT, retrying twice on failure: it
// decides whether a signature is replaced, at the end of a run whose work is lost
// if that cannot be known.
func (f *FirmirrorSyncer) readStoredJCATWithRetry(ctx context.Context) (*storedJCAT, error) {
	var jcat *storedJCAT
	err := retryStorage(ctx, func() (err error) {
		jcat, err = f.readStoredJCAT(ctx)
		return err
	})
	return jcat, err
}

// hasBlob reports whether the JCAT holds a blob for which match is true.
func (j *storedJCAT) hasBlob(match func(kind int, data string) bool) bool {
	for _, item := range *j.Items {
		for _, blob := range *item.Blobs {
			if match(*blob.Kind, *blob.Data) {
				return true
			}
		}
	}
	return false
}

// isSigned reports whether the JCAT holds a signature, not only checksums.
func (j *storedJCAT) isSigned() bool {
	return j.hasBlob(func(kind int, _ string) bool { return slices.Contains(jcatSignatureKinds, kind) })
}

// hasChecksum reports whether the JCAT holds this SHA-256 digest.
func (j *storedJCAT) hasChecksum(digest string) bool {
	return j.hasBlob(func(kind int, data string) bool { return kind == jcatKindSHA256 && data == digest })
}

// storedJCATCurrent reports whether the stored JCAT is the one of the metadata
// this run read, and signed when signing is configured.
func (f *FirmirrorSyncer) storedJCATCurrent(jcat *storedJCAT) bool {
	return jcat != nil && jcat.hasChecksum(f.stored.file) && (!f.signing() || jcat.isSigned())
}
