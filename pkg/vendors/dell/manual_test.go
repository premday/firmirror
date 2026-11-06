package dell

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func writeManual(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "manual.yaml")
	require.NoError(t, os.WriteFile(path, []byte(body), 0644))
	return path
}

func TestLoadManual(t *testing.T) {
	path := writeManual(t, `
- name: Dell PowerEdge XE9680 PCIe Switch Firmware
  system_ids: ["0B73", "0B74"]
  versions:
    - version: "4.224.0.0"
      url: https://dl.dell.com/FOLDER1M/2/Firmware_PG2CN_LN64_4.224.0.0_A00_01.BIN
      sha256: 566694134318ae382d1521fc699c7c1b449f4d1d1462e51b87c8a3a88a0a8615
      size: 19167257
`)

	entries, err := LoadManual(path)
	require.NoError(t, err)
	require.Len(t, entries, 1)
	assert.Equal(t, "Dell PowerEdge XE9680 PCIe Switch Firmware", entries[0].Name)
	assert.Equal(t, []string{"0B73", "0B74"}, entries[0].SystemIDs)
	require.Len(t, entries[0].Versions, 1)
	assert.Equal(t, "4.224.0.0", entries[0].Versions[0].Version)
	assert.Equal(t, int64(19167257), entries[0].Versions[0].Size)
}

func TestLoadManualMissingFileIsNotAnError(t *testing.T) {
	entries, err := LoadManual("")
	require.NoError(t, err)
	assert.Empty(t, entries)
}

func TestLoadManualRejectsBadEntries(t *testing.T) {
	for name, body := range map[string]string{
		"no name": `
- system_ids: ["0B73"]
  versions:
    - version: "1.0"
      url: https://dl.dell.com/x.bin
`,
		"no versions": `
- name: Something
  system_ids: ["0B73"]
`,
		"no url": `
- name: Something
  versions:
    - version: "1.0"
`,
		"bad scheme": `
- name: Something
  versions:
    - version: "1.0"
      url: ftp://dl.dell.com/x.bin
`,
		"short digest": `
- name: Something
  versions:
    - version: "1.0"
      url: https://dl.dell.com/x.bin
      sha256: abc
`,
	} {
		t.Run(name, func(t *testing.T) {
			_, err := LoadManual(writeManual(t, body))
			assert.Error(t, err)
		})
	}
}

func TestManualComponentsRespectSystemFilter(t *testing.T) {
	manual := []DellManualFirmware{{
		Name:      "Dell PowerEdge XE9680 PCIe Switch Firmware",
		SystemIDs: []string{"0B73"},
		Versions: []DellManualVersion{{
			Version: "4.224.0.0",
			URL:     "https://dl.dell.com/FOLDER1M/2/Firmware_PG2CN_LN64_4.224.0.0_A00_01.BIN",
			SHA256:  "566694134318ae382d1521fc699c7c1b449f4d1d1462e51b87c8a3a88a0a8615",
			Size:    19167257,
		}},
	}}

	// A run for another system does not take it.
	other := &DellVendor{SystemIDs: []string{"0C60"}, Manual: manual}
	assert.Empty(t, other.manualComponents())

	// A run for its system does, and the entry carries the URL and digest.
	mine := &DellVendor{SystemIDs: []string{"0B73"}, Manual: manual}
	components := mine.manualComponents()
	require.Len(t, components, 1)
	assert.Equal(t, "4.224.0.0", components[0].VendorVersion)
	assert.Equal(t, manual[0].Versions[0].URL, components[0].ManualURL)
	assert.Equal(t, manual[0].Versions[0].SHA256, components[0].Hash)
	assert.Equal(t, int64(19167257), components[0].Size)
	assert.Equal(t, "FRMW", components[0].ComponentType.Value)
}

func TestManualEntryIsPublishedAlongsideTheCatalog(t *testing.T) {
	manual := []DellManualFirmware{{
		Name:      "Dell PowerEdge XE9680 PCIe Switch Firmware",
		SystemIDs: []string{"0B73"},
		Versions: []DellManualVersion{{
			Version: "4.224.0.0",
			URL:     "https://dl.dell.com/FOLDER1M/2/Firmware_PG2CN_LN64_4.224.0.0_A00_01.BIN",
		}},
	}}
	vendor := &DellVendor{SystemIDs: []string{"0B73"}, Manual: manual}

	catalog := &DellCatalog{BaseLocation: "dl.dell.com"}
	filtered := vendor.filterCatalog(catalog)
	require.Len(t, filtered.SoftwareComponents, 1)

	entries := filtered.ListEntries()
	require.Len(t, entries, 1)
	// The entry names its own location, not one relative to the catalog.
	assert.Equal(t, manual[0].Versions[0].URL, entries[0].GetSourceURL())
	assert.Equal(t, "Firmware_PG2CN_LN64_4.224.0.0_A00_01.BIN", entries[0].GetFilename())
}
