package firmirror

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
)

// jcatKindSHA256 is libjcat's JcatBlobKind for a SHA-256 checksum.
const jcatKindSHA256 = 1

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

// hasChecksum reports whether the JCAT holds this SHA-256 digest.
func (j *storedJCAT) hasChecksum(digest string) bool {
	return j.hasBlob(func(kind int, data string) bool { return kind == jcatKindSHA256 && data == digest })
}

// storedJCATMatches reports whether the stored JCAT carries the checksum of the
// stored metadata. A missing JCAT does not.
func (f *FirmirrorSyncer) storedJCATMatches(ctx context.Context) (bool, error) {
	jcat, err := f.readStoredJCAT(ctx)
	if err != nil || jcat == nil {
		return false, err
	}
	return jcat.hasChecksum(f.stored.file), nil
}
