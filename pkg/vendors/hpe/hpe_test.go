package hpe

import (
	"archive/zip"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestNewHPEVendor(t *testing.T) {
	t.Run("PublicRepository", func(t *testing.T) {
		vendor := NewHPEVendor("test-repo", DefaultBaseURL)

		assert.NotNil(t, vendor, "Vendor should not be nil")
		expectedBaseURL := "https://downloads.linux.hpe.com/SDR/repo/test-repo"
		assert.Equal(t, expectedBaseURL, vendor.BaseURL, "BaseURL should be correctly constructed")
		assert.Equal(t, expectedBaseURL, vendor.UpstreamURL, "UpstreamURL should be the public repository")
	})

	t.Run("MirrorURL", func(t *testing.T) {
		vendor := NewHPEVendor("fwpp-gen11", "https://mirror.example.com/SDR/repo")

		assert.Equal(t, "https://mirror.example.com/SDR/repo/fwpp-gen11", vendor.BaseURL, "BaseURL should point at the mirror")
		assert.Equal(t, "https://downloads.linux.hpe.com/SDR/repo/fwpp-gen11", vendor.UpstreamURL, "Mirroring should not move the published URL")
	})

	t.Run("MirrorDirectory", func(t *testing.T) {
		// The trailing slash an rsync destination usually carries must not
		// double up in the repository path.
		vendor := NewHPEVendor("fwpp-gen11", "/srv/mirror/SDR/repo/")

		assert.Equal(t, "/srv/mirror/SDR/repo/fwpp-gen11", vendor.BaseURL, "BaseURL should point at the mirror directory")
		assert.Equal(t, "https://downloads.linux.hpe.com/SDR/repo/fwpp-gen11", vendor.UpstreamURL, "Mirroring should not move the published URL")
	})
}

func TestHPEVendor_LocalMirror(t *testing.T) {
	mirror := mockMirror(t)

	for name, baseURL := range map[string]string{"Directory": mirror, "FileURL": "file://" + mirror} {
		t.Run(name, func(t *testing.T) {
			vendor := NewHPEVendor("fwpp-gen11", baseURL)

			catalog, err := vendor.FetchCatalog(context.Background())
			assert.NoError(t, err, "FetchCatalog should read the mirror directory")

			entries := catalog.ListEntries()
			assert.Len(t, entries, 2, "Catalog should contain exactly 2 entries")

			for _, entry := range entries {
				assert.Equal(t, "https://downloads.linux.hpe.com/SDR/repo/fwpp-gen11/current/"+entry.GetFilename(),
					entry.GetSourceURL(), "The published URL should stay on the public repository")
			}

			tmpDir := t.TempDir()
			assert.NoError(t, vendor.RetrieveFirmware(context.Background(), entries[0], tmpDir),
				"RetrieveFirmware should copy from the mirror directory")

			downloaded := filepath.Join(tmpDir, entries[0].GetFilename())
			assert.FileExists(t, downloaded, "Firmware should be copied out of the mirror")
			content, err := os.ReadFile(downloaded)
			assert.NoError(t, err)
			assert.Equal(t, "Mock firmware content for "+entries[0].GetFilename(), string(content),
				"File content should match the mirrored firmware")
		})
	}
}

func TestHPEVendor_FetchCatalog(t *testing.T) {
	server := mockServer(t)
	defer server.Close()

	// Create vendor with mock server URL
	vendor := &HPEVendor{
		BaseURL: server.URL,
	}

	catalog, err := vendor.FetchCatalog(context.Background())
	assert.NoError(t, err, "FetchCatalog should not return an error")
	assert.NotNil(t, catalog, "Catalog should not be nil")

	hpeCatalog, ok := catalog.(*HPECatalog)
	assert.True(t, ok, "Catalog should be of type *HPECatalog")

	assert.Len(t, hpeCatalog.Entries, 2, "Catalog should contain exactly 2 entries")
	for filename := range hpeCatalog.Entries {
		assert.Equal(t, ".fwpkg", filepath.Ext(filename), "Only .fwpkg files should be included")
	}
	assert.Equal(t, server.URL, hpeCatalog.UpstreamURL, "A vendor with no upstream should publish where it read from")
}

func TestHPEVendor_RetrieveFirmware(t *testing.T) {
	server := mockServer(t)
	defer server.Close()

	vendor := &HPEVendor{
		BaseURL: server.URL,
	}

	// Create a temporary directory for downloads
	tmpDir := t.TempDir()

	// Create a test firmware entry
	entry := &HPEFirmwareEntry{
		Filename: "test-firmware-v1.0.0.fwpkg",
		Entry: &HPECatalogEntry{
			Date:        "20240115",
			Description: "Test firmware",
			Version:     "1.0.0",
		},
	}

	// Test retrieving firmware
	err := vendor.RetrieveFirmware(context.Background(), entry, tmpDir)
	assert.NoError(t, err, "RetrieveFirmware should not return an error")

	// Check that file was created
	expectedPath := filepath.Join(tmpDir, "test-firmware-v1.0.0.fwpkg")
	assert.FileExists(t, expectedPath, "Downloaded file should exist")
	assert.Equal(t, expectedPath, entry.DownloadPath, "Download path should be stored in entry")

	// Verify file content
	content, err := os.ReadFile(expectedPath)
	assert.NoError(t, err, "Should be able to read downloaded file")
	expectedContent := "Mock firmware content for test-firmware-v1.0.0.fwpkg"
	assert.Equal(t, expectedContent, string(content), "File content should match expected")
}

func TestHPECatalog_ListEntries(t *testing.T) {
	// Create a test catalog
	catalog := &HPECatalog{
		Entries: map[string]HPECatalogEntry{
			"firmware1.fwpkg": {
				Date:        "2024-01-15",
				Description: "First firmware",
				Version:     "1.0.0",
			},
			"firmware2.fwpkg": {
				Date:        "2024-01-20",
				Description: "Second firmware",
				Version:     "2.0.0",
			},
		},
	}

	entries := catalog.ListEntries()
	assert.Len(t, entries, 2, "Should return exactly 2 entries")

	// Check that entries are properly converted
	filenames := make([]string, len(entries))
	for i, entry := range entries {
		hpeEntry, ok := entry.(*HPEFirmwareEntry)
		assert.True(t, ok, "Entry should be of type *HPEFirmwareEntry")
		assert.NotNil(t, hpeEntry.Entry, "Entry field should not be nil")

		filenames[i] = hpeEntry.GetFilename()
	}

	assert.Contains(t, filenames, "firmware1.fwpkg", "Should contain firmware1.fwpkg")
	assert.Contains(t, filenames, "firmware2.fwpkg", "Should contain firmware2.fwpkg")
}

func TestHPEFirmwareEntry_GetFilename(t *testing.T) {
	entry := &HPEFirmwareEntry{
		Filename: "test-firmware.fwpkg",
		Entry:    &HPECatalogEntry{},
	}

	filename := entry.GetFilename()
	assert.Equal(t, "test-firmware.fwpkg", filename, "GetFilename should return the correct filename")
}

func TestHPEFirmwareEntry_ToAppstream_NotDownloaded(t *testing.T) {
	entry := &HPEFirmwareEntry{
		Filename: "test-firmware.fwpkg",
		Entry:    &HPECatalogEntry{},
		// DownloadPath is empty, should cause error
	}

	_, err := entry.ToAppstream()
	assert.Error(t, err, "Should return error when firmware not downloaded")
	assert.Contains(t, err.Error(), "firmware must be retrieved first", "Error should mention firmware must be retrieved first")
}

func TestHPEFirmwareEntry_ToAppstream_InvalidZip(t *testing.T) {
	tmpDir := t.TempDir()

	// Create invalid zip file
	invalidZipPath := filepath.Join(tmpDir, "invalid.fwpkg")
	err := os.WriteFile(invalidZipPath, []byte("not a zip file"), 0644)
	assert.NoError(t, err, "Should be able to create invalid zip file")

	entry := &HPEFirmwareEntry{
		Filename:     "invalid.fwpkg",
		Entry:        &HPECatalogEntry{},
		DownloadPath: invalidZipPath,
	}

	_, err = entry.ToAppstream()
	assert.Error(t, err, "Should return error for invalid zip file")
}

func TestHPEFirmwareEntry_ToAppstream_Success(t *testing.T) {
	tmpDir := t.TempDir()

	// Create a mock firmware zip file with payload.json
	mockFirmwarePath := createMockHPEFirmware(t, tmpDir)

	entry := &HPEFirmwareEntry{
		Filename:     "test-firmware.fwpkg",
		Entry:        &HPECatalogEntry{},
		DownloadPath: mockFirmwarePath,
	}

	components, err := entry.ToAppstream()
	assert.NoError(t, err, "ToAppstream should not return an error")
	assert.Len(t, components, 1, "Should return exactly one component")

	component := components[0]

	// Verify basic component properties
	assert.Equal(t, "firmware", component.Type, "Component type should be firmware")
	assert.Equal(t, "HPE Ethernet 100Gb 2-port QSFP56 MCX623106AS-CDAT Adapter", component.Name, "Component name should match")
	assert.Equal(t, "Hewlett Packard Enterprise", component.DeveloperName, "Developer name should be HPE")

	// Verify releases
	assert.Len(t, component.Releases, 1, "Should have exactly one release")
	release := component.Releases[0]
	assert.Equal(t, "22.41.1000", release.Version, "Release version should match")
	assert.Equal(t, "2024-06-21", release.Date, "Release date should match")
	assert.Equal(t, 360, release.InstallDuration, "Install duration should match")

	// Verify categories
	assert.Contains(t, component.Categories, "X-NetworkInterface", "Should contain X-NetworkInterface category")

	// Verify custom fields
	customKeys := make([]string, len(component.Custom))
	for i, custom := range component.Custom {
		customKeys[i] = custom.Key
	}
	assert.Contains(t, customKeys, "LVFS::DeviceFlags", "Should contain DeviceFlags custom field")
	assert.Contains(t, customKeys, "LVFS::UpdateMessage", "Should contain UpdateMessage custom field")
	assert.Contains(t, customKeys, "LVFS::UpdateProtocol", "Should contain UpdateProtocol custom field")
	assert.Contains(t, customKeys, "LVFS::DeviceIntegrity", "Should contain DeviceIntegrity custom field")

	// Verify provides section
	assert.NotEmpty(t, component.Provides, "Should have provides entries")
	assert.Equal(t, "flashed", component.Provides[0].Type, "Provides type should be flashed")
}

func TestHPEFirmwareEntry_ToAppstream_MissingPayload(t *testing.T) {
	tmpDir := t.TempDir()

	// Create zip without payload.json
	zipPath := filepath.Join(tmpDir, "no-payload.fwpkg")
	zipFile, err := os.Create(zipPath)
	assert.NoError(t, err, "Should be able to create zip file")
	defer zipFile.Close()

	zipWriter := zip.NewWriter(zipFile)

	// Add a different file, not payload.json
	otherFile, err := zipWriter.Create("other.txt")
	assert.NoError(t, err, "Should be able to create other file")
	otherFile.Write([]byte("not payload"))

	zipWriter.Close()

	entry := &HPEFirmwareEntry{
		Filename:     "no-payload.fwpkg",
		Entry:        &HPECatalogEntry{},
		DownloadPath: zipPath,
	}

	_, err = entry.ToAppstream()
	assert.Error(t, err, "Should return error when payload.json is missing")
	assert.Contains(t, err.Error(), "file not found", "Error should mention file not found")
}

// mockMirror lays out a copy of one SDR repository on disk, the way an rsync
// mirror of downloads.linux.hpe.com holds it, and returns the directory the
// fwpp-<gen> repositories sit in.
func mockMirror(t *testing.T) string {
	mirror := t.TempDir()
	current := filepath.Join(mirror, "fwpp-gen11", "current")
	assert.NoError(t, os.MkdirAll(filepath.Join(current, "fwrepodata"), 0755), "Should be able to create the mirror")

	catalog, err := os.ReadFile(filepath.Join("testdata", "catalog.json"))
	assert.NoError(t, err, "Should be able to read test catalog")
	assert.NoError(t, os.WriteFile(filepath.Join(current, "fwrepodata", "fwrepo.json"), catalog, 0644))

	var entries map[string]HPECatalogEntry
	assert.NoError(t, json.Unmarshal(catalog, &entries), "Should be able to decode test catalog")
	for filename := range entries {
		content := "Mock firmware content for " + filename
		assert.NoError(t, os.WriteFile(filepath.Join(current, filename), []byte(content), 0644))
	}

	return mirror
}

// mockServer creates a test HTTP server that serves the test catalog
func mockServer(t *testing.T) *httptest.Server {
	mux := http.NewServeMux()

	// Serve the test catalog JSON
	mux.HandleFunc("/current/fwrepodata/fwrepo.json", func(w http.ResponseWriter, r *http.Request) {
		catalogPath := filepath.Join("testdata", "catalog.json")
		content, err := os.ReadFile(catalogPath)
		if !assert.NoError(t, err, "Should be able to read test catalog") {
			http.Error(w, "Test catalog not found", http.StatusNotFound)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		w.Write(content)
	})

	// Serve mock firmware files
	mux.HandleFunc("/current/", func(w http.ResponseWriter, r *http.Request) {
		filename := filepath.Base(r.URL.Path)

		// Return mock firmware content
		mockContent := "Mock firmware content for " + filename
		w.Write([]byte(mockContent))
	})

	return httptest.NewServer(mux)
}

// Helper function to create a mock HPE firmware zip file with payload.json
func createMockHPEFirmware(t *testing.T, dir string) string {
	firmwarePath := filepath.Join(dir, "test-firmware.fwpkg")

	// Create zip file
	zipFile, err := os.Create(firmwarePath)
	assert.NoError(t, err, "Should be able to create zip file")
	defer zipFile.Close()

	zipWriter := zip.NewWriter(zipFile)
	defer zipWriter.Close()

	payloadJSON := filepath.Join("testdata", "payload.json")
	content, err := os.ReadFile(payloadJSON)
	assert.NoError(t, err, "Should be able to read test payload")

	payloadFile, err := zipWriter.Create("payload.json")
	assert.NoError(t, err, "Should be able to create payload.json in zip")

	_, err = payloadFile.Write(content)
	assert.NoError(t, err, "Should be able to write payload JSON")

	return firmwarePath
}
