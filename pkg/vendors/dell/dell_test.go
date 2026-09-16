package dell

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/xml"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

// mockMirror lays out a copy of the Dell download site on disk, the way an
// rsync mirror of it holds it, and returns the directory the catalog sits in.
func mockMirror(t *testing.T) string {
	mirror := t.TempDir()
	assert.NoError(t, os.MkdirAll(filepath.Join(mirror, "catalog"), 0755), "Should be able to create the mirror")

	catalog, err := os.ReadFile(filepath.Join("testdata", "catalog.xml"))
	assert.NoError(t, err, "Should be able to read test catalog")
	encoded := utf16LE(catalog)

	catalogFile, err := os.Create(filepath.Join(mirror, "catalog", "catalog.xml.gz"))
	assert.NoError(t, err, "Should be able to create the mirrored catalog")
	defer catalogFile.Close()

	gzipWriter := gzip.NewWriter(catalogFile)
	_, err = gzipWriter.Write(encoded)
	assert.NoError(t, err, "Should be able to gzip catalog content")
	assert.NoError(t, gzipWriter.Close())

	// The firmware sits at the path the catalog gives, below the same root.
	parsed, err := parseDellCatalog(bytes.NewReader(encoded))
	assert.NoError(t, err, "Should be able to decode test catalog")
	for _, component := range parsed.SoftwareComponents {
		path := filepath.Join(mirror, filepath.FromSlash(component.Path))
		assert.NoError(t, os.MkdirAll(filepath.Dir(path), 0755))
		content := "Mock Dell firmware content for " + filepath.Base(component.Path)
		assert.NoError(t, os.WriteFile(path, []byte(content), 0644))
	}

	return mirror
}

// utf16LE encodes the catalog the way Dell publishes it, so the parser meets
// the same bytes it meets in production.
func utf16LE(content []byte) []byte {
	encoded := []byte{0xFF, 0xFE} // BOM for UTF-16LE
	for _, b := range content {
		encoded = append(encoded, b, 0x00)
	}
	return encoded
}

// mockServer creates a test HTTP server that serves the test catalog
func mockServer(t *testing.T) *httptest.Server {
	mux := http.NewServeMux()

	// Serve the test catalog XML (gzipped)
	mux.HandleFunc("/catalog/catalog.xml.gz", func(w http.ResponseWriter, r *http.Request) {
		catalogPath := filepath.Join("testdata", "catalog.xml")
		content, err := os.ReadFile(catalogPath)
		if !assert.NoError(t, err, "Should be able to read test catalog") {
			http.Error(w, "Test catalog not found", http.StatusNotFound)
			return
		}

		// Convert to UTF-16 Little Endian with BOM as expected by Dell's parser
		utf16Content := utf16LE(content)

		// Serve as gzipped content
		w.Header().Set("Content-Type", "application/x-gzip")

		gzipWriter := gzip.NewWriter(w)
		defer gzipWriter.Close()

		_, err = gzipWriter.Write(utf16Content)
		if !assert.NoError(t, err, "Should be able to gzip catalog content") {
			return
		}
	})

	// Serve mock firmware files
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/catalog/catalog.xml.gz" {
			return // Already handled above
		}

		filename := filepath.Base(r.URL.Path)

		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Disposition", "attachment; filename="+filename)

		// Return mock firmware content
		mockContent := "Mock Dell firmware content for " + filename
		w.Write([]byte(mockContent))
	})

	return httptest.NewServer(mux)
}

func TestNewDellVendor(t *testing.T) {
	expectedBaseURL := "https://dl.dell.com"

	t.Run("WithSystemIDs", func(t *testing.T) {
		systemIDs := []string{"0C60", "0C61"}
		vendor := NewDellVendor(systemIDs, expectedBaseURL)

		assert.NotNil(t, vendor, "Vendor should not be nil")
		assert.Equal(t, expectedBaseURL, vendor.BaseURL, "BaseURL should be set correctly")
		assert.Equal(t, systemIDs, vendor.SystemIDs, "SystemIDs should be set correctly")
	})

	t.Run("WithoutSystemIDs", func(t *testing.T) {
		vendor := NewDellVendor(nil, expectedBaseURL)

		assert.NotNil(t, vendor, "Vendor should not be nil")
		assert.Equal(t, expectedBaseURL, vendor.BaseURL, "BaseURL should be set correctly")
		assert.Nil(t, vendor.SystemIDs, "SystemIDs should be nil")
	})

	t.Run("WithEmptySystemIDs", func(t *testing.T) {
		vendor := NewDellVendor([]string{}, expectedBaseURL)

		assert.NotNil(t, vendor, "Vendor should not be nil")
		assert.Equal(t, expectedBaseURL, vendor.BaseURL, "BaseURL should be set correctly")
		assert.Empty(t, vendor.SystemIDs, "SystemIDs should be empty")
	})

	t.Run("WithMirrorURL", func(t *testing.T) {
		vendor := NewDellVendor(nil, "https://mirror.example.com/dell")

		assert.Equal(t, "https://mirror.example.com/dell", vendor.BaseURL, "BaseURL should point at the mirror")
	})

	t.Run("WithMirrorDirectory", func(t *testing.T) {
		// The trailing slash an rsync destination usually carries must not
		// double up in the catalog path.
		vendor := NewDellVendor(nil, "/srv/mirror/dell/")

		assert.Equal(t, "/srv/mirror/dell", vendor.BaseURL, "BaseURL should point at the mirror directory")
	})
}

func TestDellVendor_LocalMirror(t *testing.T) {
	mirror := mockMirror(t)

	for name, baseURL := range map[string]string{"Directory": mirror, "FileURL": "file://" + mirror} {
		t.Run(name, func(t *testing.T) {
			vendor := NewDellVendor([]string{"0C60"}, baseURL)

			catalog, err := vendor.FetchCatalog(context.Background())
			assert.NoError(t, err, "FetchCatalog should read the mirror directory")

			entries := catalog.ListEntries()
			assert.NotEmpty(t, entries, "Catalog should hold the mirrored components")

			for _, entry := range entries {
				assert.Contains(t, entry.GetSourceURL(), "https://dl.dell.com/",
					"The published URL should stay on the location the catalog names")
			}

			tmpDir := t.TempDir()
			assert.NoError(t, vendor.RetrieveFirmware(context.Background(), entries[0], tmpDir),
				"RetrieveFirmware should copy from the mirror directory")

			downloaded := filepath.Join(tmpDir, entries[0].GetFilename())
			assert.FileExists(t, downloaded, "Firmware should be copied out of the mirror")
			content, err := os.ReadFile(downloaded)
			assert.NoError(t, err)
			assert.Equal(t, "Mock Dell firmware content for "+entries[0].GetFilename(), string(content),
				"File content should match the mirrored firmware")
		})
	}
}

func TestDellVendor_FetchCatalog(t *testing.T) {
	server := mockServer(t)
	defer server.Close()

	t.Run("NoSystemIDFilter", func(t *testing.T) {
		vendor := &DellVendor{
			BaseURL:   server.URL,
			SystemIDs: nil, // No filter
		}

		catalog, err := vendor.FetchCatalog(context.Background())
		assert.NoError(t, err, "FetchCatalog should not return an error")
		assert.NotNil(t, catalog, "Catalog should not be nil")

		dellCatalog, ok := catalog.(*DellCatalog)
		assert.True(t, ok, "Catalog should be of type *DellCatalog")

		// Should have the firmware and the BIOS entry (drivers filtered out)
		assert.Len(t, dellCatalog.SoftwareComponents, 2, "Should have 1 firmware and 1 BIOS component")

		// Verify only flashable components are included
		types := make([]string, len(dellCatalog.SoftwareComponents))
		for i, component := range dellCatalog.SoftwareComponents {
			assert.Contains(t, []string{"FRMW", "BIOS"}, component.ComponentType.Value, "Only firmware and BIOS components should be included")
			types[i] = component.ComponentType.Value
		}
		assert.Contains(t, types, "BIOS", "BIOS component should be mirrored, not dropped as a non-FRMW type")
		assert.NotContains(t, types, "DRVR", "Drivers should be filtered out")
	})

	t.Run("WithSystemIDFilter", func(t *testing.T) {
		vendor := &DellVendor{
			BaseURL:   server.URL,
			SystemIDs: []string{"0C60"}, // Filter for specific system
		}

		catalog, err := vendor.FetchCatalog(context.Background())
		assert.NoError(t, err, "FetchCatalog should not return an error")
		assert.NotNil(t, catalog, "Catalog should not be nil")

		dellCatalog, ok := catalog.(*DellCatalog)
		assert.True(t, ok, "Catalog should be of type *DellCatalog")

		// Should have 2 entries (the firmware and the BIOS support 0C60)
		assert.Len(t, dellCatalog.SoftwareComponents, 2, "Should have 2 components for system 0C60")
	})

	t.Run("WithNonMatchingSystemIDFilter", func(t *testing.T) {
		vendor := &DellVendor{
			BaseURL:   server.URL,
			SystemIDs: []string{"9999"}, // Non-existing system
		}

		catalog, err := vendor.FetchCatalog(context.Background())
		assert.NoError(t, err, "FetchCatalog should not return an error")
		assert.NotNil(t, catalog, "Catalog should not be nil")

		dellCatalog, ok := catalog.(*DellCatalog)
		assert.True(t, ok, "Catalog should be of type *DellCatalog")

		// Should have 0 entries
		assert.Len(t, dellCatalog.SoftwareComponents, 0, "Should have 0 components for non-matching system")
	})
}

func TestDellVendor_RetrieveFirmware(t *testing.T) {
	server := mockServer(t)
	defer server.Close()

	vendor := &DellVendor{
		BaseURL: server.URL,
	}

	tmpDir := t.TempDir()

	// Create a test firmware entry
	entry := &DellFirmwareEntry{
		Filename: "firmware1.exe",
		DellSoftwareComponent: &DellSoftwareComponent{
			Path: "FOLDER01/firmware1.exe",
			Name: DellTranslatable{
				Display: []DellTranslatableEntry{
					{Lang: "en", Value: "Test Firmware"},
				},
			},
		},
	}

	// Test retrieving firmware
	err := vendor.RetrieveFirmware(context.Background(), entry, tmpDir)
	assert.NoError(t, err, "RetrieveFirmware should not return an error")

	// Check that file was created
	expectedPath := filepath.Join(tmpDir, "firmware1.exe")
	assert.FileExists(t, expectedPath, "Downloaded file should exist")

	// Verify file content
	content, err := os.ReadFile(expectedPath)
	assert.NoError(t, err, "Should be able to read downloaded file")

	expectedContent := "Mock Dell firmware content for firmware1.exe"
	assert.Equal(t, expectedContent, string(content), "File content should match expected")
}

func TestDellCatalog_ListEntries(t *testing.T) {
	catalog := &DellCatalog{
		SoftwareComponents: []DellSoftwareComponent{
			{
				Path: "folder1/firmware1.exe",
				Name: DellTranslatable{
					Display: []DellTranslatableEntry{
						{Lang: "en", Value: "First Firmware"},
					},
				},
				ComponentType: DellTranslatableWithValue{Value: "FRMW"},
			},
			{
				Path: "folder2/firmware2.exe",
				Name: DellTranslatable{
					Display: []DellTranslatableEntry{
						{Lang: "en", Value: "Second Firmware"},
					},
				},
				ComponentType: DellTranslatableWithValue{Value: "FRMW"},
			},
		},
	}

	entries := catalog.ListEntries()
	assert.Len(t, entries, 2, "Should return exactly 2 entries")

	// Check that entries are properly converted
	filenames := make([]string, len(entries))
	for i, entry := range entries {
		dellEntry, ok := entry.(*DellFirmwareEntry)
		assert.True(t, ok, "Entry should be of type *DellFirmwareEntry")
		assert.NotNil(t, dellEntry.DellSoftwareComponent, "DellSoftwareComponent field should not be nil")

		filenames[i] = dellEntry.GetFilename()
	}

	assert.Contains(t, filenames, "firmware1.exe", "Should contain firmware1.exe")
	assert.Contains(t, filenames, "firmware2.exe", "Should contain firmware2.exe")
}

func TestDellFirmwareEntry_GetFilename(t *testing.T) {
	entry := &DellFirmwareEntry{
		Filename:              "test-firmware.exe",
		DellSoftwareComponent: &DellSoftwareComponent{},
	}

	filename := entry.GetFilename()
	assert.Equal(t, "test-firmware.exe", filename, "GetFilename should return the correct filename")
}

func TestDellFirmwareEntry_ToAppstream(t *testing.T) {
	t.Run("SingleDevice", func(t *testing.T) {
		entry := &DellFirmwareEntry{
			Filename: "test-firmware.exe",
			DellSoftwareComponent: &DellSoftwareComponent{
				Path:           "FOLDER01/test-firmware.exe",
				VendorVersion:  "1.0.0",
				DateTime:       mustParseTime("2024-01-15T10:30:00Z"),
				RebootRequired: true,
				Name: DellTranslatable{
					Display: []DellTranslatableEntry{
						{Lang: "en", Value: "Test Network Firmware"},
					},
				},
				Description: DellTranslatable{
					Display: []DellTranslatableEntry{
						{Lang: "en", Value: "Test firmware description"},
					},
				},
				ImportantInfo: DellTranslatable{
					Display: []DellTranslatableEntry{
						{Lang: "en", Value: "Reboot required"},
					},
				},
				LUCategory: DellTranslatableWithValue{
					Value: "Network",
				},
				Criticality: DellCriticality{
					Value: 1, // Medium urgency
				},
				SupportedSystems: []DellBrand{
					{
						Models: []DellModel{
							{SystemID: "0C60"},
						},
					},
				},
				SupportedDevices: []DellDevice{
					{
						ComponentID:      "DEV001",
						DellTranslatable: DellTranslatable{Display: []DellTranslatableEntry{{Lang: "en", Value: "Network Device 1"}}},
					},
				},
			},
		}

		components, err := entry.ToAppstream()
		assert.NoError(t, err, "ToAppstream should not return an error")
		assert.Len(t, components, 1, "Should return exactly one component for one device")

		component := components[0]

		// Name should be the device name, summary should be the firmware package name
		assert.Equal(t, "firmware", component.Type, "Component type should be firmware")
		assert.Equal(t, "proprietary", component.MetadataLicense, "Metadata license should be proprietary")
		assert.Equal(t, "proprietary", component.ProjectLicense, "Project license should be proprietary")
		assert.Equal(t, "Network Device 1", component.Name, "Component name should be the device name")
		assert.Equal(t, "Test Network Firmware", component.Summary, "Component summary should be the firmware package name")
		assert.Equal(t, "<p>Test firmware description</p>", component.Description.Value, "Component description should match")

		// Verify releases
		assert.Len(t, component.Releases, 1, "Should have exactly one release")
		release := component.Releases[0]
		assert.Equal(t, "1.0.0", release.Version, "Release version should match")
		assert.Equal(t, "medium", release.Urgency, "Release urgency should be medium for criticality 1")

		// Verify categories for Network LUCategory
		assert.Contains(t, component.Categories, "X-NetworkInterface", "Should contain X-NetworkInterface category")

		// Verify custom fields for reboot required
		customKeys := make([]string, len(component.Custom))
		for i, custom := range component.Custom {
			customKeys[i] = custom.Key
		}
		assert.Contains(t, customKeys, "LVFS::DeviceFlags", "Should contain DeviceFlags custom field")
		assert.Contains(t, customKeys, "LVFS::UpdateMessage", "Should contain UpdateMessage custom field")
		assert.Contains(t, customKeys, "LVFS::UpdateProtocol", "Should contain UpdateProtocol custom field")
		assert.Contains(t, customKeys, "LVFS::DeviceIntegrity", "Should contain DeviceIntegrity custom field")

		// Verify provides section - should only have GUIDs for this device
		assert.Len(t, component.Provides, 1, "Should have exactly 1 provides entry (1 device x 1 system)")
	})

	t.Run("MultipleDevices", func(t *testing.T) {
		entry := &DellFirmwareEntry{
			Filename: "test-firmware.exe",
			DellSoftwareComponent: &DellSoftwareComponent{
				Path:           "FOLDER01/test-firmware.exe",
				VendorVersion:  "1.0.0",
				DateTime:       mustParseTime("2024-01-15T10:30:00Z"),
				RebootRequired: false,
				Name: DellTranslatable{
					Display: []DellTranslatableEntry{
						{Lang: "en", Value: "Multi-Device Firmware"},
					},
				},
				Description: DellTranslatable{
					Display: []DellTranslatableEntry{
						{Lang: "en", Value: "Firmware for multiple devices"},
					},
				},
				LUCategory: DellTranslatableWithValue{
					Value: "SAS Drive",
				},
				Criticality: DellCriticality{
					Value: 2, // Critical
				},
				SupportedSystems: []DellBrand{
					{
						Models: []DellModel{
							{SystemID: "0C60"},
							{SystemID: "0C61"},
						},
					},
				},
				SupportedDevices: []DellDevice{
					{
						ComponentID:      "DEV001",
						DellTranslatable: DellTranslatable{Display: []DellTranslatableEntry{{Lang: "en", Value: "SAS Drive Model A"}}},
					},
					{
						ComponentID:      "DEV002",
						DellTranslatable: DellTranslatable{Display: []DellTranslatableEntry{{Lang: "en", Value: "SAS Drive Model B"}}},
					},
				},
			},
		}

		components, err := entry.ToAppstream()
		assert.NoError(t, err, "ToAppstream should not return an error")
		assert.Len(t, components, 2, "Should return one component per device")

		// First component - DEV001
		assert.Equal(t, "SAS Drive Model A", components[0].Name, "First component name should be first device name")
		assert.Equal(t, "Multi-Device Firmware", components[0].Summary, "Summary should be the firmware package name")
		assert.Equal(t, "<p>Firmware for multiple devices</p>", components[0].Description.Value)
		assert.Len(t, components[0].Provides, 2, "First component should have 2 provides (1 device x 2 systems)")
		assert.Contains(t, components[0].Categories, "X-Drive", "Should contain X-Drive category")
		assert.Equal(t, "critical", components[0].Releases[0].Urgency, "Urgency should be critical")

		// Second component - DEV002
		assert.Equal(t, "SAS Drive Model B", components[1].Name, "Second component name should be second device name")
		assert.Equal(t, "Multi-Device Firmware", components[1].Summary, "Summary should be the firmware package name")
		assert.Len(t, components[1].Provides, 2, "Second component should have 2 provides (1 device x 2 systems)")

		// Component IDs should differ (based on componentID)
		assert.NotEqual(t, components[0].ID, components[1].ID, "Component IDs should be different for different devices")
	})

	t.Run("DescriptionWithAmpersand", func(t *testing.T) {
		entry := &DellFirmwareEntry{
			Filename: "test-firmware.exe",
			DellSoftwareComponent: &DellSoftwareComponent{
				Path:          "FOLDER01/test-firmware.exe",
				VendorVersion: "1.0.0",
				DateTime:      mustParseTime("2024-01-15T10:30:00Z"),
				Name: DellTranslatable{
					Display: []DellTranslatableEntry{
						{Lang: "en", Value: "Security & Management Firmware"},
					},
				},
				Description: DellTranslatable{
					Display: []DellTranslatableEntry{
						{Lang: "en", Value: "Fixes for CVE-2024-1234 & CVE-2024-5678"},
					},
				},
				LUCategory:  DellTranslatableWithValue{Value: "BIOS"},
				Criticality:  DellCriticality{Value: 2},
				SupportedSystems: []DellBrand{
					{Models: []DellModel{{SystemID: "0C60"}}},
				},
				SupportedDevices: []DellDevice{
					{
						ComponentID:      "BIOS001",
						DellTranslatable: DellTranslatable{Display: []DellTranslatableEntry{{Lang: "en", Value: "System BIOS"}}},
					},
				},
			},
		}

		components, err := entry.ToAppstream()
		assert.NoError(t, err)
		assert.Len(t, components, 1)

		component := components[0]
		assert.Equal(t, "<p>Fixes for CVE-2024-1234 &amp; CVE-2024-5678</p>", component.Description.Value,
			"Ampersands in description should be XML-escaped")
		assert.Equal(t, "Security & Management Firmware", component.Summary,
			"Summary should preserve raw ampersand (XML marshaler handles escaping)")

		// Verify the XML round-trips correctly
		xmlBytes, err := xml.MarshalIndent(component, "", "  ")
		assert.NoError(t, err, "XML marshaling should succeed")
		assert.Contains(t, string(xmlBytes), "&amp;", "Serialized XML should contain escaped ampersand")
	})

	t.Run("NoDevices", func(t *testing.T) {
		entry := &DellFirmwareEntry{
			Filename: "test-firmware.exe",
			DellSoftwareComponent: &DellSoftwareComponent{
				Path:          "FOLDER01/test-firmware.exe",
				VendorVersion: "1.0.0",
				DateTime:      mustParseTime("2024-01-15T10:30:00Z"),
				Name: DellTranslatable{
					Display: []DellTranslatableEntry{
						{Lang: "en", Value: "Orphan Firmware"},
					},
				},
				Description: DellTranslatable{
					Display: []DellTranslatableEntry{
						{Lang: "en", Value: "Firmware with no devices"},
					},
				},
				LUCategory:       DellTranslatableWithValue{Value: "BIOS"},
				Criticality:      DellCriticality{Value: 3},
				SupportedDevices: []DellDevice{},
			},
		}

		components, err := entry.ToAppstream()
		assert.NoError(t, err, "ToAppstream should not return an error")
		assert.Empty(t, components, "Should return no components when there are no devices")
	})
}

// The C6615 and R6625 iDRAC packages share device component ID 25227 but apply
// to different systems, so they must not collapse into one component.
func TestDellFirmwareEntry_ToAppstreamPerPlatformIdentity(t *testing.T) {
	idracEntry := func(version string, systemIDs ...string) *DellFirmwareEntry {
		models := make([]DellModel, 0, len(systemIDs))
		for _, systemID := range systemIDs {
			models = append(models, DellModel{SystemID: systemID})
		}
		return &DellFirmwareEntry{
			Filename: "idrac-" + version + ".exe",
			DellSoftwareComponent: &DellSoftwareComponent{
				Path:          "FOLDER01/idrac-" + version + ".exe",
				VendorVersion: version,
				DateTime:      mustParseTime("2026-01-15T10:30:00Z"),
				Name: DellTranslatable{
					Display: []DellTranslatableEntry{{Lang: "en", Value: "iDRAC " + version}},
				},
				Description: DellTranslatable{
					Display: []DellTranslatableEntry{{Lang: "en", Value: "iDRAC firmware"}},
				},
				LUCategory:       DellTranslatableWithValue{Value: "iDRAC with Lifecycle Controller"},
				Criticality:      DellCriticality{Value: 1},
				SupportedSystems: []DellBrand{{Models: models}},
				SupportedDevices: []DellDevice{{
					ComponentID:      "25227",
					DellTranslatable: DellTranslatable{Display: []DellTranslatableEntry{{Lang: "en", Value: "iDRAC with Lifecycle Controller"}}},
				}},
			},
		}
	}

	// In the real catalog 7.30.30.51 covers the C6615 (0C60), 7.30.30.54 does not.
	c6615, err := idracEntry("7.30.30.51", "0C60", "0900").ToAppstream()
	assert.NoError(t, err)
	r6625, err := idracEntry("7.30.30.54", "0AF8", "0B53").ToAppstream()
	assert.NoError(t, err)

	assert.NotEqual(t, c6615[0].ID, r6625[0].ID,
		"Packages for the same device on different platforms must be distinct components")
	assert.NotEqual(t, c6615[0].Provides, r6625[0].Provides)

	t.Run("StableAcrossVersionsForTheSamePlatforms", func(t *testing.T) {
		next, err := idracEntry("7.40.00.00", "0C60", "0900").ToAppstream()
		assert.NoError(t, err)
		assert.Equal(t, c6615[0].ID, next[0].ID,
			"A newer package for the same platforms must stay the same component so its release supersedes the previous one")
	})

	t.Run("IndependentOfSystemOrder", func(t *testing.T) {
		reordered, err := idracEntry("7.30.30.51", "0900", "0C60").ToAppstream()
		assert.NoError(t, err)
		assert.Equal(t, c6615[0].ID, reordered[0].ID, "Component identity should not depend on catalog ordering")
		assert.Equal(t, c6615[0].Provides, reordered[0].Provides, "GUIDs should be emitted in a stable order")
	})
}

// Helper function for parsing time in tests
func mustParseTime(timeStr string) time.Time {
	t, err := time.Parse(time.RFC3339, timeStr)
	if err != nil {
		panic(err)
	}
	return t
}
