package dell

import (
	"bytes"
	"encoding/xml"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/premday/firmirror/pkg/lvfs"
)

// componentOfType returns the first filtered component of that Dell type.
func componentOfType(t *testing.T, catalog *DellCatalog, componentType string) *DellSoftwareComponent {
	t.Helper()

	for i := range catalog.SoftwareComponents {
		if catalog.SoftwareComponents[i].ComponentType.Value == componentType {
			return &catalog.SoftwareComponents[i]
		}
	}

	t.Fatalf("no %s entry in catalog", componentType)
	return nil
}

func TestGoldenAppStream(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("testdata", "catalog.xml"))
	if err != nil {
		t.Fatalf("reading catalog.xml: %v", err)
	}

	var catalog DellCatalog
	decoder := xml.NewDecoder(bytes.NewReader(data))
	decoder.CharsetReader = func(charset string, input io.Reader) (io.Reader, error) {
		return input, nil
	}
	if err := decoder.Decode(&catalog); err != nil {
		t.Fatalf("parsing catalog: %v", err)
	}

	filtered := (&DellVendor{}).filterCatalog(&catalog)

	for _, tc := range []struct {
		name          string
		componentType string
		golden        string
	}{
		{"Firmware", "FRMW", "golden.xml"},
		{"BIOS", "BIOS", "golden_bios.xml"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fw := componentOfType(t, filtered, tc.componentType)
			entry := &DellFirmwareEntry{
				Filename:              filepath.Base(fw.Path),
				DellSoftwareComponent: fw,
			}

			components, err := entry.ToAppstream()
			if err != nil {
				t.Fatalf("ToAppstream failed: %v", err)
			}

			lvfs.AssertGoldenComponents(t, filepath.Join("testdata", tc.golden), components)
		})
	}
}
