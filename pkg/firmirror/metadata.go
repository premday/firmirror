package firmirror

import (
	"bytes"
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path"
	"path/filepath"

	"github.com/klauspost/compress/zstd"
	"github.com/premday/firmirror/pkg/lvfs"
)

// IndexKey is the metadata document refresh maintains: every firmware mirrored
// so far, and the state every other document is derived from.
const (
	IndexKey        = "metadata.xml.zst"
	signatureSuffix = ".jcat"
)

// readComponents parses the metadata document stored at key, reporting whether
// it exists at all.
func (f *FirmirrorSyncer) readComponents(ctx context.Context, key string) (*lvfs.Components, bool, error) {
	exists, err := f.Storage.Exists(ctx, key)
	if err != nil {
		return nil, false, fmt.Errorf("failed to check %s existence: %w", key, err)
	}
	if !exists {
		return nil, false, nil
	}

	reader, err := f.Storage.Read(ctx, key)
	if err != nil {
		return nil, false, fmt.Errorf("failed to read %s: %w", key, err)
	}
	defer reader.Close()

	zstReader, err := zstd.NewReader(reader)
	if err != nil {
		return nil, false, fmt.Errorf("failed to create zstd reader for %s: %w", key, err)
	}
	defer zstReader.Close()

	data, err := io.ReadAll(zstReader)
	if err != nil {
		return nil, false, fmt.Errorf("failed to decompress %s: %w", key, err)
	}

	var components lvfs.Components
	if err := xml.Unmarshal(data, &components); err != nil {
		return nil, false, fmt.Errorf("failed to parse %s: %w", key, err)
	}
	return &components, true, nil
}

// encodeMetadata renders the components as the compressed document published
// to storage. Components are already ordered by the caller and marshalling is
// deterministic, so an unchanged repository encodes to identical bytes.
func encodeMetadata(components *lvfs.Components) ([]byte, error) {
	out := []byte(xml.Header)
	xmlBytes, err := xml.MarshalIndent(components, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("failed to marshal metadata XML: %w", err)
	}
	out = append(out, xmlBytes...)

	var compressed bytes.Buffer
	zstWriter, err := zstd.NewWriter(&compressed)
	if err != nil {
		return nil, fmt.Errorf("failed to create zstd writer: %w", err)
	}
	if _, err := zstWriter.Write(out); err != nil {
		return nil, fmt.Errorf("failed to compress metadata: %w", err)
	}
	if err := zstWriter.Close(); err != nil {
		return nil, fmt.Errorf("failed to finalize zstd compression: %w", err)
	}
	return compressed.Bytes(), nil
}

// storedMetadataMatches reports whether key already holds exactly these bytes.
// A storage error answers no: rewriting a document is harmless, skipping a
// needed write is not.
func (f *FirmirrorSyncer) storedMetadataMatches(ctx context.Context, key string, compressed []byte) bool {
	exists, err := f.Storage.Exists(ctx, key)
	if err != nil || !exists {
		return false
	}
	reader, err := f.Storage.Read(ctx, key)
	if err != nil {
		return false
	}
	defer reader.Close()
	stored, err := io.ReadAll(reader)
	if err != nil {
		return false
	}
	return bytes.Equal(stored, compressed)
}

// writeSignedMetadata uploads a metadata document together with its JCAT. The
// document goes first because the JCAT is derived from it: a signature
// uploaded for bytes whose own upload then failed advertises a checksum for a
// document no client can fetch, while a document whose JCAT did not make it is
// repaired by a later run, which recreates the signature even when the
// metadata itself did not change.
func (f *FirmirrorSyncer) writeSignedMetadata(ctx context.Context, key string, compressed []byte) error {
	// jcat-tool works on files, and a JCAT item is identified by the file it
	// was created from, so the temporary file is named after the published
	// object rather than reusing one name for every document.
	name := path.Base(key)
	localPath := filepath.Join(f.Config.CacheDir, name)
	if err := os.WriteFile(localPath, compressed, 0644); err != nil {
		return fmt.Errorf("failed to write %s to the cache directory: %w", name, err)
	}
	defer os.Remove(localPath)

	// Always create a JCAT file containing checksums. When signing keys are
	// configured, signMetadata also adds a PKCS#7 signature.
	signaturePath := localPath + signatureSuffix
	// jcat-tool imports into the JCAT it is handed rather than replacing it,
	// so one left in the cache directory by a killed run would carry this
	// document's checksum next to a stale one.
	if err := os.Remove(signaturePath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("failed to remove the stale %s JCAT from the cache directory: %w", name, err)
	}
	if err := f.signMetadata(ctx, signaturePath, localPath); err != nil {
		return fmt.Errorf("creating %s JCAT: %w", name, err)
	}
	defer os.Remove(signaturePath)

	signature, err := os.ReadFile(signaturePath)
	if err != nil {
		return fmt.Errorf("failed to read %s JCAT file: %w", name, err)
	}
	if err := f.Storage.Write(ctx, key, bytes.NewReader(compressed)); err != nil {
		return fmt.Errorf("failed to write %s to storage: %w", name, err)
	}
	if err := f.Storage.Write(ctx, key+signatureSuffix, bytes.NewReader(signature)); err != nil {
		return fmt.Errorf("failed to write %s JCAT to storage: %w", name, err)
	}
	return nil
}

func countReleases(components *lvfs.Components) int {
	if components == nil {
		return 0
	}
	count := 0
	for _, component := range components.Component {
		count += len(component.Releases)
	}
	return count
}

// signMetadata creates a JCAT file for the given file using jcat-tool.
// The JCAT file contains a SHA256 checksum and a signature if signing keys are provided.
func (f *FirmirrorSyncer) signMetadata(ctx context.Context, sigPath, filePath string) error {
	jcatTool := func(args []string, wd string) error {
		slog.Debug("Running jcat-tool", "args", args)
		cmd := exec.CommandContext(ctx, "jcat-tool", args...)
		cmd.Dir = wd
		output, err := cmd.CombinedOutput()
		if err != nil {
			return fmt.Errorf("jcat-tool failed: %w\nOutput: %s", err, output)
		}
		return nil
	}

	wd := filepath.Dir(filePath)
	file := filepath.Base(filePath)
	sig := filepath.Base(sigPath)

	// A checksum-only JCAT is still required when cryptographic signing is
	// disabled, notably because firmware.jcat is included in every CAB.
	if err := jcatTool([]string{"self-sign", sig, file, "--kind", "sha256"}, wd); err != nil {
		return fmt.Errorf("failed to create JCAT file with checksums: %w", err)
	}

	if f.Config.Certificate != "" && f.Config.PrivateKey != "" {
		// Add a signature to the JCAT file using the certificate and private key.
		// with GPG:
		//   gpg --detach-sign --sign --armor firmware.xml.zst
		//   jcat-tool import firmware.xml.zst.jcat firmware.xml.zst firmware.xml.zst.asc
		if err := jcatTool([]string{"sign", sig, file, f.Config.Certificate, f.Config.PrivateKey}, wd); err != nil {
			return fmt.Errorf("failed to add signature to JCAT file: %w", err)
		}
	}

	return nil
}
