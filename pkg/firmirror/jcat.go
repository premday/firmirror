package firmirror

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"slices"
)

// jcatKindSHA256 is libjcat's JcatBlobKind for a SHA-256 checksum.
const jcatKindSHA256 = 1

// jcatSignatureKinds are libjcat's blob kinds for GPG, PKCS#7 and ED25519
// signatures.
var jcatSignatureKinds = []int{2, 3, 9}

// storedJCAT is the part of a JCAT firmirror reads. libjcat writes gzipped JSON:
// one item per file, holding blobs of a kind (checksum, signature) and their
// data, which for a checksum is its hex digest.
type storedJCAT struct {
	Items []struct {
		Blobs []struct {
			Kind int
			Data string
		}
	}
}

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
		return nil, fmt.Errorf("reading the stored JCAT, which is not a compressed JCAT: %w", err)
	}
	defer uncompressed.Close()

	var jcat storedJCAT
	if err := json.NewDecoder(uncompressed).Decode(&jcat); err != nil {
		return nil, fmt.Errorf("reading the stored JCAT: %w", err)
	}
	return &jcat, nil
}

// hasBlob reports whether the JCAT holds a blob for which match is true.
func (j *storedJCAT) hasBlob(match func(kind int, data string) bool) bool {
	for _, item := range j.Items {
		for _, blob := range item.Blobs {
			if match(blob.Kind, blob.Data) {
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

// jcatState is how the stored JCAT stands against the stored metadata.
type jcatState int

const (
	// jcatStale is missing, not the metadata's, or unsigned when signing is
	// configured.
	jcatStale jcatState = iota
	jcatCurrent
	// jcatRepublished is for the file stored now, not the one this run read:
	// another run published meanwhile.
	jcatRepublished
)

// storedJCATState compares the stored JCAT with the metadata this run read, then
// with the one stored now.
func (f *FirmirrorSyncer) storedJCATState(ctx context.Context) (jcatState, error) {
	jcat, err := f.readStoredJCAT(ctx)
	if err != nil || jcat == nil {
		return jcatStale, err
	}
	if jcat.hasChecksum(f.stored.file) {
		if f.signing() && !jcat.isSigned() {
			return jcatStale, nil
		}
		return jcatCurrent, nil
	}

	compressed, exists, err := f.readStoredFile(ctx)
	if err != nil || !exists {
		return jcatStale, err
	}
	current := fileChecksum(compressed)
	if current == f.stored.file || !jcat.hasChecksum(current) {
		return jcatStale, nil
	}
	return jcatRepublished, nil
}
