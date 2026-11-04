package dell

import (
	"archive/zip"
	"encoding/xml"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/text/encoding/unicode"
	"golang.org/x/text/transform"
)

// payloadDir is where a DUP keeps its firmware images.
const payloadDir = "payload/"

// openDUP opens the ZIP appended to a Dell DUP. The archive's central-directory
// offsets are absolute file offsets, so archive/zip reads the DUP directly with
// no base offset.
func openDUP(path string) (*zip.ReadCloser, error) {
	zr, err := zip.OpenReader(path)
	if err != nil {
		return nil, fmt.Errorf("opening DUP %s: %w", path, err)
	}
	return zr, nil
}

// parseDUPPackage decodes package.xml, which is UTF-16LE.
func parseDUPPackage(r io.Reader) (*DellDUPPackage, error) {
	// The XML decoder only reads UTF-8, so convert the UTF-16 to UTF-8.
	unicodeReader := transform.NewReader(r, unicode.BOMOverride(unicode.UTF16(unicode.LittleEndian, unicode.IgnoreBOM).NewDecoder()))

	var pkg DellDUPPackage

	xmlDecoder := xml.NewDecoder(unicodeReader)
	xmlDecoder.CharsetReader = func(charset string, input io.Reader) (io.Reader, error) {
		// The XML declaration still names UTF-16; the decoder must not try to
		// honour it after the transform above.
		return input, nil
	}
	if err := xmlDecoder.Decode(&pkg); err != nil {
		return nil, fmt.Errorf("parsing DUP package.xml: %w", err)
	}

	return &pkg, nil
}

// extractPayloads writes every payload/*.signed member into dir and returns the
// written file names in archive order.
func extractPayloads(zr *zip.ReadCloser, dir string) ([]string, error) {
	var names []string
	found := false

	for _, f := range zr.File {
		if !strings.HasPrefix(f.Name, payloadDir) || strings.HasSuffix(f.Name, "/") {
			continue
		}
		found = true

		name := filepath.Base(f.Name)
		if err := extractMember(f, filepath.Join(dir, name)); err != nil {
			return nil, err
		}
		names = append(names, name)
	}

	if !found {
		return nil, fmt.Errorf("no %s members in DUP", payloadDir)
	}
	return names, nil
}

// extractMember streams one archive member to dest. The members are ~1 MiB, so
// they are copied rather than read whole.
func extractMember(f *zip.File, dest string) error {
	rc, err := f.Open()
	if err != nil {
		return fmt.Errorf("opening DUP member %s: %w", f.Name, err)
	}
	defer rc.Close()

	out, err := os.Create(dest)
	if err != nil {
		return fmt.Errorf("creating %s: %w", dest, err)
	}

	if _, err := io.Copy(out, rc); err != nil {
		out.Close()
		return fmt.Errorf("extracting DUP member %s: %w", f.Name, err)
	}
	if err := out.Close(); err != nil {
		return fmt.Errorf("closing %s: %w", dest, err)
	}
	return nil
}

// dupPackage reads package.xml out of an open DUP.
func dupPackage(zr *zip.ReadCloser) (*DellDUPPackage, error) {
	for _, f := range zr.File {
		if f.Name != "package.xml" {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return nil, fmt.Errorf("opening DUP package.xml: %w", err)
		}
		defer rc.Close()
		return parseDUPPackage(rc)
	}
	return nil, fmt.Errorf("no package.xml in DUP")
}
