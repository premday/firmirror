package firmirror

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/klauspost/compress/zstd"
	"github.com/premday/firmirror/pkg/lvfs"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMain(m *testing.M) {
	binDir, err := os.MkdirTemp("", "firmirror-test-bin-")
	if err != nil {
		panic(err)
	}
	jcatTool := filepath.Join(binDir, "jcat-tool")
	script := `#!/bin/sh
if [ -n "$JCAT_TOOL_LOG" ]; then
	printf '%s\n' "$*" >> "$JCAT_TOOL_LOG"
fi
if [ "$1" = "self-sign" ]; then
	: > "$2"
fi
`
	if err := os.WriteFile(jcatTool, []byte(script), 0755); err != nil {
		panic(err)
	}
	fwupdTool := filepath.Join(binDir, "fwupdtool")
	fwupdScript := `#!/bin/sh
if [ "$1" != "build-cabinet" ]; then
	exit 1
fi
output="$2"
shift 2
for input in "$@"; do
	test -f "$input" || exit 1
done
: > "$output"
`
	if err := os.WriteFile(fwupdTool, []byte(fwupdScript), 0755); err != nil {
		panic(err)
	}
	if err := os.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH")); err != nil {
		panic(err)
	}

	code := m.Run()
	os.RemoveAll(binDir)
	os.Exit(code)
}

// createTestSyncer creates a test configuration with local storage
func createTestSyncer(t *testing.T) (*FirmirrorSyncer, string) {
	tmpDir := t.TempDir()
	storage, err := NewLocalStorage(filepath.Join(tmpDir, "output"))
	require.NoError(t, err)
	config := FirmirrorConfig{
		CacheDir:       filepath.Join(tmpDir, "cache"),
		Certificate:    "", // No certificate for tests
		PrivateKey:     "", // No private key for tests
		MaxConcurrency: 2,
	}

	syncer, err := NewFirmirrorSyncer(config, storage)
	require.NoError(t, err)
	return syncer, tmpDir
}

type cleanupStorage struct {
	*LocalStorage
	deleteFailures map[string]int
}

func (s *cleanupStorage) List(_ context.Context, prefix string) ([]string, error) {
	entries, err := os.ReadDir(s.basePath)
	if err != nil {
		return nil, err
	}
	keys := make([]string, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasPrefix(entry.Name(), prefix) {
			keys = append(keys, entry.Name())
		}
	}
	return keys, nil
}

func (s *cleanupStorage) Delete(_ context.Context, key string) error {
	if s.deleteFailures[key] > 0 {
		s.deleteFailures[key]--
		return errors.New("transient delete failure")
	}
	return os.Remove(filepath.Join(s.basePath, key))
}

// MockVendor implements the Vendor interface for testing
type MockVendor struct {
	catalog         *MockCatalog
	fetchErr        error
	retrieveErr     error
	mu              sync.Mutex
	retrievedFiles  []string
	retrieveContent string
}

func (m *MockVendor) FetchCatalog(ctx context.Context) (Catalog, error) {
	if m.fetchErr != nil {
		return nil, m.fetchErr
	}
	return m.catalog, nil
}

func (m *MockVendor) RetrieveFirmware(ctx context.Context, entry FirmwareEntry, tmpDir string) error {
	if m.retrieveErr != nil {
		return m.retrieveErr
	}

	filename := entry.GetFilename()
	filepath := filepath.Join(tmpDir, filename)

	content := m.retrieveContent
	if content == "" {
		content = "mock firmware content"
	}

	err := os.WriteFile(filepath, []byte(content), 0644)
	if err != nil {
		return err
	}

	m.mu.Lock()
	m.retrievedFiles = append(m.retrievedFiles, filename)
	m.mu.Unlock()
	return nil
}

// MockCatalog implements the Catalog interface for testing
type MockCatalog struct {
	entries []FirmwareEntry
}

func (m *MockCatalog) ListEntries() []FirmwareEntry {
	return m.entries
}

// MockFirmwareEntry implements the FirmwareEntry interface for testing
type MockFirmwareEntry struct {
	filename     string
	sourceURL    string
	components   []lvfs.Component
	appstreamErr error
}

func (m *MockFirmwareEntry) GetFilename() string {
	return m.filename
}

func (m *MockFirmwareEntry) GetSourceURL() string {
	return m.sourceURL
}

func (m *MockFirmwareEntry) ToAppstream() ([]lvfs.Component, error) {
	if m.appstreamErr != nil {
		return nil, m.appstreamErr
	}
	return m.components, nil
}

func TestNewFirmirrorSyncer(t *testing.T) {
	t.Run("CreatesSyncerWithConfig", func(t *testing.T) {
		syncer, _ := createTestSyncer(t)

		assert.NotNil(t, syncer, "Syncer should not be nil")
		assert.NotNil(t, syncer.Storage, "Storage should be set")
		assert.NotNil(t, syncer.vendors, "Vendors map should be initialized")
		assert.Empty(t, syncer.vendors, "Vendors map should be empty initially")
	})
}

func TestFirmirrorSyncer_RegisterVendor(t *testing.T) {
	t.Run("RegistersVendors", func(t *testing.T) {
		syncer, _ := createTestSyncer(t)
		mockVendor1 := &MockVendor{}
		mockVendor2 := &MockVendor{}

		syncer.RegisterVendor("vendor1", mockVendor1)
		syncer.RegisterVendor("vendor2", mockVendor2)

		vendors := syncer.GetAllVendors()
		assert.Len(t, vendors, 2, "Should have two vendors registered")
		assert.Contains(t, vendors, "vendor1", "Should contain vendor1")
		assert.Contains(t, vendors, "vendor2", "Should contain vendor2")
	})
}

func TestFirmirrorSyncer_GetAllVendors(t *testing.T) {
	t.Run("ReturnsClone", func(t *testing.T) {
		syncer, _ := createTestSyncer(t)
		mockVendor := &MockVendor{}

		syncer.RegisterVendor("test-vendor", mockVendor)

		vendors := syncer.GetAllVendors()

		// Modify the returned map
		delete(vendors, "test-vendor")

		// Original should still have the vendor
		originalVendors := syncer.GetAllVendors()
		assert.Len(t, originalVendors, 1, "Original vendors map should be unchanged")
		assert.Contains(t, originalVendors, "test-vendor", "Original should still contain test-vendor")
	})
}

func TestFirmirrorSyncer_ProcessVendor(t *testing.T) {
	t.Run("SuccessfulProcessing", func(t *testing.T) {
		syncer, _ := createTestSyncer(t)

		// Create mock firmware entry with minimal AppStream component
		mockEntry := &MockFirmwareEntry{
			filename: "test-firmware.bin",
			components: []lvfs.Component{
				{
					Type:            "firmware",
					ID:              "com.test.firmware",
					Name:            "Test Firmware",
					Summary:         "Test firmware summary",
					MetadataLicense: "proprietary",
					ProjectLicense:  "proprietary",
					Releases: []lvfs.Release{
						{
							Version: "1.0.0",
						},
					},
				},
			},
		}

		mockVendor := &MockVendor{
			catalog: &MockCatalog{
				entries: []FirmwareEntry{mockEntry},
			},
		}

		_ = syncer.ProcessVendor(context.TODO(), mockVendor, "test-vendor")

		// Note: This will fail because fwupdtool is not available in test environment
		// But we can verify the vendor methods were called
		assert.NotNil(t, mockVendor.retrievedFiles, "Firmware retrieval should be attempted")
	})

	t.Run("FetchCatalogError", func(t *testing.T) {
		syncer, _ := createTestSyncer(t)

		mockVendor := &MockVendor{
			fetchErr: errors.New("catalog fetch failed"),
		}

		err := syncer.ProcessVendor(context.TODO(), mockVendor, "test-vendor")

		assert.Error(t, err, "Should return error when catalog fetch fails")
		assert.Contains(t, err.Error(), "catalog fetch failed", "Error should contain original error message")
	})

	t.Run("RetrieveFirmwareError", func(t *testing.T) {
		syncer, _ := createTestSyncer(t)

		mockEntry := &MockFirmwareEntry{
			filename: "test-firmware.bin",
		}

		mockVendor := &MockVendor{
			catalog: &MockCatalog{
				entries: []FirmwareEntry{mockEntry},
			},
			retrieveErr: errors.New("retrieve failed"),
		}

		err := syncer.ProcessVendor(context.TODO(), mockVendor, "test-vendor")

		assert.Error(t, err, "ProcessVendor should return error when all entries fail")
		assert.Contains(t, err.Error(), "all 1 firmware entries failed")
	})

	t.Run("ToAppstreamError", func(t *testing.T) {
		syncer, _ := createTestSyncer(t)

		mockEntry := &MockFirmwareEntry{
			filename:     "test-firmware.bin",
			appstreamErr: errors.New("appstream conversion failed"),
		}

		mockVendor := &MockVendor{
			catalog: &MockCatalog{
				entries: []FirmwareEntry{mockEntry},
			},
		}

		err := syncer.ProcessVendor(context.TODO(), mockVendor, "test-vendor")

		assert.Error(t, err, "ProcessVendor should return error when all entries fail")
		assert.Contains(t, err.Error(), "all 1 firmware entries failed")
		assert.Len(t, mockVendor.retrievedFiles, 1, "Firmware should be retrieved before appstream conversion")
	})

	t.Run("EmptyCatalog", func(t *testing.T) {
		syncer, _ := createTestSyncer(t)

		mockVendor := &MockVendor{
			catalog: &MockCatalog{
				entries: []FirmwareEntry{},
			},
		}

		err := syncer.ProcessVendor(context.TODO(), mockVendor, "test-vendor")

		assert.NoError(t, err, "Should succeed with empty catalog")
		assert.Empty(t, mockVendor.retrievedFiles, "No firmware should be retrieved")
	})

	t.Run("MultipleFirmwareEntries", func(t *testing.T) {
		syncer, _ := createTestSyncer(t)

		mockEntry1 := &MockFirmwareEntry{
			filename: "firmware1.bin",
			components: []lvfs.Component{
				{
					Type:            "firmware",
					ID:              "com.test.firmware1",
					Name:            "Test Firmware 1",
					MetadataLicense: "proprietary",
				},
			},
		}

		mockEntry2 := &MockFirmwareEntry{
			filename: "firmware2.bin",
			components: []lvfs.Component{
				{
					Type:            "firmware",
					ID:              "com.test.firmware2",
					Name:            "Test Firmware 2",
					MetadataLicense: "proprietary",
				},
			},
		}

		mockVendor := &MockVendor{
			catalog: &MockCatalog{
				entries: []FirmwareEntry{mockEntry1, mockEntry2},
			},
		}

		syncer.ProcessVendor(context.TODO(), mockVendor, "test-vendor")

		assert.Len(t, mockVendor.retrievedFiles, 2, "Both firmware files should be retrieved")
		assert.Contains(t, mockVendor.retrievedFiles, "firmware1.bin", "Should retrieve firmware1")
		assert.Contains(t, mockVendor.retrievedFiles, "firmware2.bin", "Should retrieve firmware2")
	})

	t.Run("TempDirectoryCreated", func(t *testing.T) {
		syncer, tmpDir := createTestSyncer(t)

		mockEntry := &MockFirmwareEntry{
			filename: "test-firmware.bin",
			components: []lvfs.Component{
				{
					Type:            "firmware",
					ID:              "com.test.firmware",
					MetadataLicense: "proprietary",
				},
			},
		}

		mockVendor := &MockVendor{
			catalog: &MockCatalog{
				entries: []FirmwareEntry{mockEntry},
			},
		}

		syncer.ProcessVendor(context.TODO(), mockVendor, "test-vendor")

		entries, err := os.ReadDir(tmpDir)
		require.NoError(t, err, "Should be able to read output directory")
		assert.NotNil(t, entries, "Output directory should exist")
	})
}

func TestFirmirrorSyncer_BuildPackage(t *testing.T) {
	t.Run("CreatesMetainfoXML", func(t *testing.T) {
		syncer, tmpDir := createTestSyncer(t)

		components := []*lvfs.Component{
			{
				Type:            "firmware",
				ID:              "com.test.firmware",
				Name:            "Test Firmware",
				Summary:         "Test summary",
				MetadataLicense: "proprietary",
				ProjectLicense:  "proprietary",
			},
		}

		// Create a dummy firmware file
		firmwareFilename := "firmware.bin"
		firmwareFile := filepath.Join(tmpDir, firmwareFilename)
		err := os.WriteFile(firmwareFile, []byte("test firmware"), 0644)
		require.NoError(t, err, "Should create test firmware file")

		require.NoError(t, syncer.buildPackage(context.TODO(), components, firmwareFilename, tmpDir))

		// Verify metainfo XML was created (named 0.metainfo.xml)
		metainfoPath := filepath.Join(tmpDir, "0.metainfo.xml")
		assert.FileExists(t, metainfoPath, "Metainfo XML should be created")

		// Verify XML content
		content, err := os.ReadFile(metainfoPath)
		require.NoError(t, err, "Should be able to read metainfo XML")

		assert.Contains(t, string(content), "<?xml version", "Should contain XML header")
		assert.Contains(t, string(content), "com.test.firmware", "Should contain component ID")
		assert.Contains(t, string(content), "Test Firmware", "Should contain component name")
	})
}

func TestFirmirrorSyncer_SignMetadata(t *testing.T) {
	t.Run("CreatesChecksumJCATWithoutSigningKeys", func(t *testing.T) {
		syncer, tmpDir := createTestSyncer(t)
		payloadPath := filepath.Join(tmpDir, "firmware.bin")
		signaturePath := filepath.Join(tmpDir, "firmware.jcat")
		logPath := filepath.Join(tmpDir, "jcat-tool.log")
		require.NoError(t, os.WriteFile(payloadPath, []byte("firmware"), 0644))
		t.Setenv("JCAT_TOOL_LOG", logPath)

		require.NoError(t, syncer.signMetadata(context.Background(), signaturePath, payloadPath))
		assert.FileExists(t, signaturePath)
		log, err := os.ReadFile(logPath)
		require.NoError(t, err)
		assert.Equal(t, "self-sign firmware.jcat firmware.bin --kind sha256\n", string(log))
	})

	t.Run("AddsSignatureWhenSigningKeysAreConfigured", func(t *testing.T) {
		syncer, tmpDir := createTestSyncer(t)
		syncer.Config.Certificate = "/secrets/signing.cert"
		syncer.Config.PrivateKey = "/secrets/signing.key"
		payloadPath := filepath.Join(tmpDir, "firmware.bin")
		signaturePath := filepath.Join(tmpDir, "firmware.jcat")
		logPath := filepath.Join(tmpDir, "jcat-tool.log")
		require.NoError(t, os.WriteFile(payloadPath, []byte("firmware"), 0644))
		t.Setenv("JCAT_TOOL_LOG", logPath)

		require.NoError(t, syncer.signMetadata(context.Background(), signaturePath, payloadPath))
		log, err := os.ReadFile(logPath)
		require.NoError(t, err)
		commands := strings.Split(strings.TrimSpace(string(log)), "\n")
		require.Len(t, commands, 2)
		assert.Equal(t, "self-sign firmware.jcat firmware.bin --kind sha256", commands[0])
		assert.Equal(t, "sign firmware.jcat firmware.bin /secrets/signing.cert /secrets/signing.key", commands[1])
	})
}

func TestFirmirrorSyncer_LoadMetadata(t *testing.T) {
	t.Run("LoadsExistingMetadata", func(t *testing.T) {
		tmpDir := t.TempDir()
		storage, err := NewLocalStorage(tmpDir)
		require.NoError(t, err)

		syncer, err := NewFirmirrorSyncer(FirmirrorConfig{
			CacheDir:    filepath.Join(tmpDir, "cache"),
			Certificate: "",
			PrivateKey:  "",
		}, storage)
		require.NoError(t, err)

		// Create test metadata
		testComponents := &lvfs.Components{
			Origin:        "firmirror",
			SchemaVersion: lvfs.MetadataSchemaVersion,
			Component: []lvfs.Component{
				{
					Type:            "firmware",
					ID:              "com.test.firmware1",
					Name:            "Test Firmware 1",
					MetadataLicense: "proprietary",
					Releases: []lvfs.Release{
						{
							Version: "1.0.0",
							Checksums: []lvfs.Checksum{
								{
									Filename: "firmware1.bin",
									Type:     "sha256",
									Value:    "abc123",
								},
							},
						},
					},
				},
				{
					Type:            "firmware",
					ID:              "com.test.firmware2",
					Name:            "Test Firmware 2",
					MetadataLicense: "proprietary",
					Releases: []lvfs.Release{
						{
							Version: "2.0.0",
							Checksums: []lvfs.Checksum{
								{
									Filename: "firmware2.bin",
									Type:     "sha256",
									Value:    "def456",
								},
							},
						},
					},
				},
			},
		}

		// Write compressed metadata file
		metadataPath := filepath.Join(tmpDir, "metadata.xml.zst")
		file, err := os.Create(metadataPath)
		require.NoError(t, err)

		zstWriter, err := zstd.NewWriter(file)
		require.NoError(t, err)

		xmlData, err := xml.Marshal(testComponents)
		require.NoError(t, err)
		_, err = zstWriter.Write([]byte(xml.Header))
		require.NoError(t, err)
		_, err = zstWriter.Write(xmlData)
		require.NoError(t, err)
		zstWriter.Close()
		file.Close()

		// Load metadata
		err = syncer.LoadMetadata(context.TODO())
		require.NoError(t, err, "Should load metadata successfully")

		// Verify loaded metadata
		assert.NotNil(t, syncer.existingMetadata, "Existing metadata should be loaded")
		assert.Len(t, syncer.existingMetadata.Component, 2, "Should have 2 components")
		assert.Equal(t, "com.test.firmware1", syncer.existingMetadata.Component[0].ID)
		assert.Equal(t, "com.test.firmware2", syncer.existingMetadata.Component[1].ID)

		// Verify index was built
		assert.Len(t, syncer.existingIndex, 2, "Should have 2 entries in index")
		assert.True(t, syncer.existingIndex["firmware1.bin"], "Should have firmware1 in index")
		assert.True(t, syncer.existingIndex["firmware2.bin"], "Should have firmware2 in index")
	})

	t.Run("HandlesNonExistentMetadata", func(t *testing.T) {
		syncer, _ := createTestSyncer(t)

		// No metadata file exists
		err := syncer.LoadMetadata(context.TODO())
		assert.NoError(t, err, "Should not error when metadata doesn't exist")
		assert.Nil(t, syncer.existingMetadata, "Existing metadata should be nil")
		assert.Empty(t, syncer.existingIndex, "Index should be empty")
	})

	t.Run("SchemaVersionMismatchClearsIndex", func(t *testing.T) {
		tmpDir := t.TempDir()
		storage, err := NewLocalStorage(tmpDir)
		require.NoError(t, err)

		syncer, err := NewFirmirrorSyncer(FirmirrorConfig{
			CacheDir:    filepath.Join(tmpDir, "cache"),
			Certificate: "",
			PrivateKey:  "",
		}, storage)
		require.NoError(t, err)

		testComponents := &lvfs.Components{
			Origin:        "firmirror",
			SchemaVersion: 0,
			Component: []lvfs.Component{
				{
					Type:            "firmware",
					ID:              "com.test.firmware1",
					Name:            "Test Firmware 1",
					MetadataLicense: "proprietary",
					Releases: []lvfs.Release{
						{
							Version: "1.0.0",
							Checksums: []lvfs.Checksum{
								{
									Filename: "firmware1.bin",
									Type:     "sha256",
									Value:    "abc123",
								},
							},
						},
					},
				},
			},
		}

		metadataPath := filepath.Join(tmpDir, "metadata.xml.zst")
		file, err := os.Create(metadataPath)
		require.NoError(t, err)

		zstWriter, err := zstd.NewWriter(file)
		require.NoError(t, err)

		xmlData, err := xml.Marshal(testComponents)
		require.NoError(t, err)
		_, err = zstWriter.Write([]byte(xml.Header))
		require.NoError(t, err)
		_, err = zstWriter.Write(xmlData)
		require.NoError(t, err)
		zstWriter.Close()
		file.Close()

		err = syncer.LoadMetadata(context.TODO())
		require.NoError(t, err, "Should load metadata successfully")

		assert.NotNil(t, syncer.existingMetadata, "Existing metadata should be loaded as fallback")
		assert.Empty(t, syncer.existingIndex, "Index should be empty when schema version mismatches, forcing reprocessing")
	})

	t.Run("HandlesCorruptedMetadata", func(t *testing.T) {
		tmpDir := t.TempDir()
		storage, err := NewLocalStorage(tmpDir)
		require.NoError(t, err)
		syncer, err := NewFirmirrorSyncer(FirmirrorConfig{
			CacheDir:    filepath.Join(tmpDir, "cache"),
			Certificate: "",
			PrivateKey:  "",
		}, storage)
		require.NoError(t, err)

		// Create corrupted metadata file
		metadataPath := filepath.Join(tmpDir, "metadata.xml.zst")
		err = os.WriteFile(metadataPath, []byte("not valid zstd"), 0644)
		require.NoError(t, err)

		// Load should fail
		err = syncer.LoadMetadata(context.TODO())
		assert.Error(t, err, "Should error with corrupted metadata")
		// zstd error message may vary but should indicate invalid input
		assert.Contains(t, err.Error(), "failed to", "Error should indicate failure")
	})
}

func TestMergeComponents(t *testing.T) {
	existing := &lvfs.Components{Component: []lvfs.Component{
		{
			ID: "com.test.rebuilt",
			Releases: []lvfs.Release{{
				Location:  "old-rebuilt.cab",
				Checksums: []lvfs.Checksum{{Filename: "firmware.bin"}},
				Artifacts: []lvfs.Artifact{{Location: "old-rebuilt.cab"}},
			}},
		},
		{
			ID: "com.test.unrelated",
			Releases: []lvfs.Release{{
				Location:  "unrelated.cab",
				Checksums: []lvfs.Checksum{{Filename: "firmware.bin"}},
			}},
		},
	}}
	incoming := []lvfs.Component{{
		ID: "com.test.rebuilt",
		Releases: []lvfs.Release{{
			Checksums: []lvfs.Checksum{{Filename: "firmware.bin"}},
			Artifacts: []lvfs.Artifact{{Location: "new-rebuilt.cab"}},
		}},
	}}

	for _, test := range []struct {
		name                string
		replaceSuperseded   bool
		wantRebuiltPackages []string
	}{
		{name: "retains superseded releases by default", wantRebuiltPackages: []string{"old-rebuilt.cab", "new-rebuilt.cab"}},
		{name: "replaces superseded releases during cleanup", replaceSuperseded: true, wantRebuiltPackages: []string{"new-rebuilt.cab"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			components, superseded := mergeComponents(existing, incoming, test.replaceSuperseded)

			var rebuiltPackages []string
			for _, release := range components["com.test.rebuilt"].Releases {
				if release.Location != "" {
					rebuiltPackages = append(rebuiltPackages, release.Location)
				} else {
					rebuiltPackages = append(rebuiltPackages, release.Artifacts[0].Location)
				}
			}
			assert.Equal(t, test.wantRebuiltPackages, rebuiltPackages)
			assert.Equal(t, "unrelated.cab", components["com.test.unrelated"].Releases[0].Location)
			assert.Equal(t, []string{"old-rebuilt.cab"}, superseded)
		})
	}
}

func TestLogRetainedSupersededPackages(t *testing.T) {
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))

	logRetainedSupersededPackages(logger, []string{"old-rebuilt.cab"})

	assert.Contains(t, logs.String(), "Leaving superseded firmware package untouched because --s3.cleanup was not specified")
	assert.Contains(t, logs.String(), "location=old-rebuilt.cab")
}

func TestFirmirrorSyncer_SaveMetadata(t *testing.T) {
	t.Run("SavesNewComponents", func(t *testing.T) {
		tmpDir := t.TempDir()
		storage, err := NewLocalStorage(tmpDir)
		require.NoError(t, err)
		syncer, err := NewFirmirrorSyncer(FirmirrorConfig{
			CacheDir:    filepath.Join(tmpDir, "cache"),
			Certificate: "",
			PrivateKey:  "",
		}, storage)
		require.NoError(t, err)

		// Add new components
		syncer.newComponents = []lvfs.Component{
			{
				Type:            "firmware",
				ID:              "com.test.firmware1",
				Name:            "Test Firmware 1",
				MetadataLicense: "proprietary",
				Releases: []lvfs.Release{
					{
						Version: "1.0.0",
						Checksums: []lvfs.Checksum{
							{
								Filename: "firmware1.bin",
								Type:     "sha256",
								Value:    "abc123",
							},
						},
						Artifacts: []lvfs.Artifact{
							{
								Type:     "binary",
								Location: "abc123abc123abc123abc123abc123abc123abc123abc123abc123abc123abc1-firmware1.bin.cab",
								Checksums: []lvfs.Checksum{
									{
										Type:  "sha256",
										Value: "abc123abc123abc123abc123abc123abc123abc123abc123abc123abc123abc1",
									},
								},
							},
						},
					},
				},
			},
		}

		// Save metadata
		err = syncer.SaveMetadata(context.TODO())
		require.NoError(t, err, "Should save metadata successfully")

		// Verify compressed file exists
		metadataZstPath := filepath.Join(tmpDir, "metadata.xml.zst")
		assert.FileExists(t, metadataZstPath, "Compressed metadata should exist")
		assert.FileExists(t, metadataZstPath+".jcat", "Checksum JCAT should exist without signing keys")

		// Read and verify content
		file, err := os.Open(metadataZstPath)
		require.NoError(t, err)
		defer file.Close()

		zstReader, err := zstd.NewReader(file)
		require.NoError(t, err)
		defer zstReader.Close()

		var components lvfs.Components
		decoder := xml.NewDecoder(zstReader)
		err = decoder.Decode(&components)
		require.NoError(t, err, "Should decode saved metadata")

		assert.Len(t, components.Component, 1, "Should have 1 component")
		assert.Equal(t, "com.test.firmware1", components.Component[0].ID)
		assert.Equal(t, "firmirror", components.Origin)
		assert.Equal(t, lvfs.MetadataSchemaVersion, components.SchemaVersion, "SchemaVersion should be set in saved metadata")

		// Verify location tag was added
		assert.Regexp(t, `^[a-f0-9]{64}-firmware1\.bin\.cab$`, components.Component[0].Releases[0].Location,
			"Should have location tag set")
	})

	t.Run("MergesExistingAndNewComponents", func(t *testing.T) {
		tmpDir := t.TempDir()
		storage, err := NewLocalStorage(tmpDir)
		require.NoError(t, err)
		syncer, err := NewFirmirrorSyncer(FirmirrorConfig{
			CacheDir:    filepath.Join(tmpDir, "cache"),
			Certificate: "",
			PrivateKey:  "",
		}, storage)
		require.NoError(t, err)

		// Set existing metadata
		syncer.existingMetadata = &lvfs.Components{
			Component: []lvfs.Component{
				{
					Type: "firmware",
					ID:   "com.existing.firmware",
					Name: "Existing Firmware",
					Releases: []lvfs.Release{
						{
							Version:  "1.0.0",
							Location: "existing.bin.cab",
							Checksums: []lvfs.Checksum{
								{Filename: "existing.bin"},
							},
						},
					},
				},
			},
		}

		// Add new components
		syncer.newComponents = []lvfs.Component{
			{
				Type: "firmware",
				ID:   "com.new.firmware",
				Name: "New Firmware",
				Releases: []lvfs.Release{
					{
						Version: "2.0.0",
						Checksums: []lvfs.Checksum{
							{Filename: "new.bin"},
						},
						Artifacts: []lvfs.Artifact{
							{
								Type:     "binary",
								Location: "def456def456def456def456def456def456def456def456def456def456def4-new.bin.cab",
								Checksums: []lvfs.Checksum{
									{
										Type:  "sha256",
										Value: "def456def456def456def456def456def456def456def456def456def456def4",
									},
								},
							},
						},
					},
				},
			},
		}

		// Save metadata
		err = syncer.SaveMetadata(context.TODO())
		require.NoError(t, err)

		// Read and verify merged content
		metadataZstPath := filepath.Join(tmpDir, "metadata.xml.zst")
		file, err := os.Open(metadataZstPath)
		require.NoError(t, err)
		defer file.Close()

		zstReader, err := zstd.NewReader(file)
		require.NoError(t, err)
		defer zstReader.Close()

		var components lvfs.Components
		decoder := xml.NewDecoder(zstReader)
		err = decoder.Decode(&components)
		require.NoError(t, err)

		assert.Len(t, components.Component, 2, "Should have 2 components (existing + new)")

		// Find components by ID
		var existingFound, newFound bool
		for _, comp := range components.Component {
			if comp.ID == "com.existing.firmware" {
				existingFound = true
				assert.Equal(t, "existing.bin.cab", comp.Releases[0].Location)
			}
			if comp.ID == "com.new.firmware" {
				newFound = true
				assert.Regexp(t, `^[a-f0-9]{64}-new\.bin\.cab$`, comp.Releases[0].Location)
			}
		}
		assert.True(t, existingFound, "Should have existing component")
		assert.True(t, newFound, "Should have new component")
	})

	t.Run("MergesReleasesForSameComponentID", func(t *testing.T) {
		tmpDir := t.TempDir()
		storage, err := NewLocalStorage(tmpDir)
		require.NoError(t, err)
		syncer, err := NewFirmirrorSyncer(FirmirrorConfig{
			CacheDir:    filepath.Join(tmpDir, "cache"),
			Certificate: "",
			PrivateKey:  "",
		}, storage)
		require.NoError(t, err)

		// Set existing metadata with a component
		syncer.existingMetadata = &lvfs.Components{
			Component: []lvfs.Component{
				{
					Type: "firmware",
					ID:   "com.test.firmware",
					Name: "Test Firmware",
					Releases: []lvfs.Release{
						{
							Version: "1.0.0",
							Checksums: []lvfs.Checksum{
								{Filename: "firmware-v1.bin"},
							},
						},
					},
				},
			},
		}

		// Add new release for the same component ID
		syncer.newComponents = []lvfs.Component{
			{
				Type: "firmware",
				ID:   "com.test.firmware",
				Name: "Test Firmware",
				Releases: []lvfs.Release{
					{
						Version: "2.0.0",
						Checksums: []lvfs.Checksum{
							{Filename: "firmware-v2.bin"},
						},
					},
				},
			},
		}

		// Save metadata
		err = syncer.SaveMetadata(context.TODO())
		require.NoError(t, err)

		// Read and verify merged content
		metadataZstPath := filepath.Join(tmpDir, "metadata.xml.zst")
		file, err := os.Open(metadataZstPath)
		require.NoError(t, err)
		defer file.Close()

		zstReader, err := zstd.NewReader(file)
		require.NoError(t, err)
		defer zstReader.Close()

		var components lvfs.Components
		decoder := xml.NewDecoder(zstReader)
		err = decoder.Decode(&components)
		require.NoError(t, err)

		assert.Len(t, components.Component, 1, "Should have 1 component")
		assert.Equal(t, "com.test.firmware", components.Component[0].ID)
		assert.Len(t, components.Component[0].Releases, 2, "Should have 2 releases")

		// Verify both releases are present
		versions := []string{}
		for _, release := range components.Component[0].Releases {
			versions = append(versions, release.Version)
		}
		assert.Contains(t, versions, "1.0.0", "Should have version 1.0.0")
		assert.Contains(t, versions, "2.0.0", "Should have version 2.0.0")
	})

	t.Run("RetriesS3CleanupOnNoOpRun", func(t *testing.T) {
		tmpDir := t.TempDir()
		local, err := NewLocalStorage(tmpDir)
		require.NoError(t, err)
		storage := &cleanupStorage{
			LocalStorage:   local,
			deleteFailures: map[string]int{"old-firmware.cab": 1},
		}
		require.NoError(t, os.WriteFile(filepath.Join(tmpDir, "old-firmware.cab"), []byte("old"), 0644))
		require.NoError(t, os.WriteFile(filepath.Join(tmpDir, "new-firmware.cab"), []byte("new"), 0644))

		syncer, err := NewFirmirrorSyncer(FirmirrorConfig{
			CacheDir:                    filepath.Join(tmpDir, "cache"),
			CleanupUnreferencedPackages: true,
		}, storage)
		require.NoError(t, err)
		syncer.existingMetadata = &lvfs.Components{Component: []lvfs.Component{{
			ID: "com.test.firmware",
			Releases: []lvfs.Release{{
				Version: "1.0.0", Location: "old-firmware.cab",
				Checksums: []lvfs.Checksum{{Filename: "firmware.bin"}},
			}},
		}}}
		syncer.newComponents = []lvfs.Component{{
			ID: "com.test.firmware",
			Releases: []lvfs.Release{{
				Version:   "1.0.0",
				Checksums: []lvfs.Checksum{{Filename: "firmware.bin"}},
				Artifacts: []lvfs.Artifact{{Location: "new-firmware.cab"}},
			}},
		}}

		require.ErrorContains(t, syncer.SaveMetadata(context.Background()), "transient delete failure")
		assert.FileExists(t, filepath.Join(tmpDir, "old-firmware.cab"))

		retrySyncer, err := NewFirmirrorSyncer(FirmirrorConfig{
			CacheDir:                    filepath.Join(tmpDir, "retry-cache"),
			CleanupUnreferencedPackages: true,
		}, storage)
		require.NoError(t, err)
		require.NoError(t, retrySyncer.LoadMetadata(context.Background()))
		require.NoError(t, retrySyncer.SaveMetadata(context.Background()))
		assert.NoFileExists(t, filepath.Join(tmpDir, "old-firmware.cab"))
		assert.FileExists(t, filepath.Join(tmpDir, "new-firmware.cab"))
	})

	t.Run("SkipsCleanupWhenDisabledAndNoComponentsAreNew", func(t *testing.T) {
		tmpDir := t.TempDir()
		local, err := NewLocalStorage(tmpDir)
		require.NoError(t, err)
		storage := &cleanupStorage{LocalStorage: local, deleteFailures: make(map[string]int)}
		require.NoError(t, os.WriteFile(filepath.Join(tmpDir, "old-firmware.cab"), []byte("old"), 0644))
		syncer, err := NewFirmirrorSyncer(FirmirrorConfig{CacheDir: filepath.Join(tmpDir, "cache")}, storage)
		require.NoError(t, err)

		require.NoError(t, syncer.SaveMetadata(context.Background()))
		assert.NoFileExists(t, filepath.Join(tmpDir, "metadata.xml.zst"))
		assert.FileExists(t, filepath.Join(tmpDir, "old-firmware.cab"))
	})

	t.Run("AddsLocationTagsToReleases", func(t *testing.T) {
		tmpDir := t.TempDir()
		storage, err := NewLocalStorage(tmpDir)
		require.NoError(t, err)
		syncer, err := NewFirmirrorSyncer(FirmirrorConfig{
			CacheDir:    filepath.Join(tmpDir, "cache"),
			Certificate: "",
			PrivateKey:  "",
		}, storage)
		require.NoError(t, err)

		// Add components without location tags
		syncer.newComponents = []lvfs.Component{
			{
				Type: "firmware",
				ID:   "com.test.firmware",
				Releases: []lvfs.Release{
					{
						Version:  "1.0.0",
						Location: "", // No location
						Checksums: []lvfs.Checksum{
							{Filename: "firmware.bin"},
						},
						Artifacts: []lvfs.Artifact{
							{
								Type:     "binary",
								Location: "1234567890abcdef1234567890abcdef1234567890abcdef1234567890abcdef-firmware.bin.cab",
								Checksums: []lvfs.Checksum{
									{
										Type:  "sha256",
										Value: "1234567890abcdef1234567890abcdef1234567890abcdef1234567890abcdef",
									},
								},
							},
						},
					},
					{
						Version:  "2.0.0",
						Location: "already-set.cab", // Already has location
						Checksums: []lvfs.Checksum{
							{Filename: "firmware-v2.bin"},
						},
					},
				},
			},
		}

		// Save metadata
		err = syncer.SaveMetadata(context.TODO())
		require.NoError(t, err)

		// Read and verify
		metadataZstPath := filepath.Join(tmpDir, "metadata.xml.zst")
		file, err := os.Open(metadataZstPath)
		require.NoError(t, err)
		defer file.Close()

		zstReader, err := zstd.NewReader(file)
		require.NoError(t, err)
		defer zstReader.Close()

		var components lvfs.Components
		decoder := xml.NewDecoder(zstReader)
		err = decoder.Decode(&components)
		require.NoError(t, err)

		// Verify locations
		releases := components.Component[0].Releases
		assert.Regexp(t, `^[a-f0-9]{64}-firmware\.bin\.cab$`, releases[0].Location,
			"Should add location based on checksum filename")
		assert.Equal(t, "already-set.cab", releases[1].Location,
			"Should preserve existing location")
	})
}
