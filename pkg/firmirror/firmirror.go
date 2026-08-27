package firmirror

import (
	"bytes"
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/klauspost/compress/zstd"
	"github.com/premday/firmirror/pkg/lvfs"
)

type FirmirrorConfig struct {
	CacheDir                    string // Local cache directory for temporary work
	Certificate                 string // Path to certificate file for signing metadata (.pem or .crt)
	PrivateKey                  string // Path to private key file for signing metadata (.pem or .key)
	MaxConcurrency              int    // Maximum number of firmware entries processed concurrently (default 1)
	CleanupUnreferencedPackages bool   // Replace rebuilt metadata releases and delete unreferenced stored CAB packages
}

type FirmirrorSyncer struct {
	Config           FirmirrorConfig
	Storage          Storage
	vendors          map[string]Vendor
	existingMetadata *lvfs.Components // Loaded metadata from existing metadata.xml.gz
	existingIndex    map[string]bool  // Index of firmware already in metadata (by filename)
	newComponents    []lvfs.Component // Components accumulated during this run
}

func NewFirmirrorSyncer(config FirmirrorConfig, storage Storage) (*FirmirrorSyncer, error) {
	// Create cache directory if it doesn't exist
	if err := os.MkdirAll(config.CacheDir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create cache directory %s: %w", config.CacheDir, err)
	}

	cleanStaleWorkDirs(config.CacheDir)

	return &FirmirrorSyncer{
		Config:        config,
		Storage:       storage,
		vendors:       make(map[string]Vendor),
		existingIndex: make(map[string]bool),
	}, nil
}

func cleanStaleWorkDirs(cacheDir string) {
	entries, err := os.ReadDir(cacheDir)
	if err != nil {
		return
	}
	for _, entry := range entries {
		if entry.IsDir() && filepath.Ext(entry.Name()) == ".wrk" {
			path := filepath.Join(cacheDir, entry.Name())
			slog.Info("Cleaning stale work directory from previous run", "path", path)
			os.RemoveAll(path)
		}
	}
}

// RegisterVendor registers a vendor with the given name
func (f *FirmirrorSyncer) RegisterVendor(name string, vendor Vendor) {
	f.vendors[name] = vendor
}

// GetAllVendors returns all registered vendors
func (f *FirmirrorSyncer) GetAllVendors() map[string]Vendor {
	// Return a copy to prevent external modifications
	return maps.Clone(f.vendors)
}

// GetNewComponentCount returns the number of new components accumulated during this run
func (f *FirmirrorSyncer) GetNewComponentCount() int {
	return len(f.newComponents)
}

// ProcessVendor processes firmware for a given vendor using the interface.
// Entries are processed concurrently up to Config.MaxConcurrency workers.
func (f *FirmirrorSyncer) ProcessVendor(ctx context.Context, vendor Vendor, vendorName string) error {
	logger := slog.With("vendor", vendorName)
	logger.Debug("Fetching catalog")

	catalog, err := vendor.FetchCatalog(ctx)
	if err != nil {
		return fmt.Errorf("failed to fetch catalog for vendor %s: %w", vendorName, err)
	}

	entries := catalog.ListEntries()
	total := len(entries)

	sem := make(chan struct{}, f.Config.MaxConcurrency)
	var wg sync.WaitGroup
	var mu sync.Mutex
	var processed atomic.Int64
	var skipped int
	var errors atomic.Int64
	entryNum := 0

	for _, entry := range entries {
		fwName := entry.GetFilename()
		entryNum++

		// Check if firmware is already in metadata index (read-only map, safe for concurrent reads)
		if f.existingIndex[fwName] {
			logger.Info("Skipping firmware already in index", "progress", fmt.Sprintf("[%d/%d]", entryNum, total), "firmware", fwName)
			skipped++
			continue
		}

		// Acquire semaphore slot, respecting context cancellation
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
		}
		if ctx.Err() != nil {
			break
		}

		wg.Add(1)
		go func(entryNum int) {
			defer wg.Done()
			defer func() { <-sem }()

			components := f.processEntry(ctx, vendor, vendorName, entry, fwName, fmt.Sprintf("[%d/%d]", entryNum, total), logger)
			if components == nil {
				errors.Add(1)
				return
			}

			mu.Lock()
			f.newComponents = append(f.newComponents, components...)
			mu.Unlock()
			processed.Add(1)
		}(entryNum)
	}

	wg.Wait()

	p := processed.Load()
	s := skipped
	e := errors.Load()
	logger.Info("Completed vendor processing", "processed", p, "skipped", s, "errors", e, "total", len(entries))
	if e > 0 && p == 0 {
		return fmt.Errorf("all %d firmware entries failed for vendor %s", e, vendorName)
	}
	return nil
}

// processEntry handles downloading, converting and packaging a single firmware entry.
// Returns the resulting components, or nil on error.
func (f *FirmirrorSyncer) processEntry(ctx context.Context, vendor Vendor, vendorName string, entry FirmwareEntry, fwName string, progress string, logger *slog.Logger) []lvfs.Component {
	entryLogger := logger.With("firmware", fwName, "progress", progress)
	entryLogger.Info("Processing firmware")
	start := time.Now()

	tmpDir := filepath.Join(f.Config.CacheDir, vendorName+"-"+fwName+".wrk")
	os.RemoveAll(tmpDir)
	if err := os.MkdirAll(tmpDir, 0755); err != nil {
		entryLogger.Error("Failed to create temp directory", "error", err)
		return nil
	}

	t0 := time.Now()
	if err := vendor.RetrieveFirmware(ctx, entry, tmpDir); err != nil {
		entryLogger.Error("Failed to retrieve firmware", "error", err)
		os.RemoveAll(tmpDir)
		return nil
	}
	entryLogger.Info("Downloaded firmware", "duration", time.Since(t0).Round(time.Millisecond))

	// Convert to AppStream components (one firmware entry may produce multiple components)
	components, err := entry.ToAppstream()
	if err != nil {
		entryLogger.Error("Failed to convert firmware", "error", err)
		os.RemoveAll(tmpDir)
		return nil
	}

	if len(components) == 0 {
		entryLogger.Info("No components produced, skipping")
		os.RemoveAll(tmpDir)
		return nil
	}

	sourceURL := entry.GetSourceURL()
	componentPtrs := make([]*lvfs.Component, len(components))
	for i := range components {
		componentPtrs[i] = &components[i]
		if sourceURL != "" {
			componentPtrs[i].URL = lvfs.URL{
				Type: "homepage",
				Text: sourceURL,
			}
		}
	}

	// Build a single package containing all component metainfo XMLs
	t0 = time.Now()
	if err = f.buildPackage(ctx, componentPtrs, fwName, tmpDir); err != nil {
		entryLogger.Error("Failed to build package", "error", err)
		os.RemoveAll(tmpDir)
		return nil
	}
	entryLogger.Info("Built and uploaded package", "duration", time.Since(t0).Round(time.Millisecond))
	os.RemoveAll(tmpDir)

	result := make([]lvfs.Component, len(componentPtrs))
	for i, comp := range componentPtrs {
		result[i] = *comp
	}

	entryLogger.Info("Completed firmware", "total_duration", time.Since(start).Round(time.Millisecond))
	return result
}

func (f *FirmirrorSyncer) buildPackage(ctx context.Context, components []*lvfs.Component, fwFile, tmpDir string) error {
	fwPath := filepath.Join(tmpDir, fwFile)
	logger := slog.With("firmware", fwFile)

	// Calculate firmware checksums (shared by all components)
	sha1Hash, sha256Hash, err := calculateChecksums(fwPath)
	if err != nil {
		return fmt.Errorf("calculating firmware checksums: %w", err)
	}

	// Write a metainfo XML per component, add firmware checksums to each
	var metainfoPaths []string
	for i, component := range components {
		for j := range component.Releases {
			component.Releases[j].Checksums = []lvfs.Checksum{
				{Filename: fwFile, Target: "content", Type: "sha1", Value: sha1Hash},
				{Filename: fwFile, Target: "content", Type: "sha256", Value: sha256Hash},
			}
		}

		metaName := fmt.Sprintf("%d.metainfo.xml", i)
		metaPath := filepath.Join(tmpDir, metaName)
		outBytes := []byte(xml.Header)
		xmlBytes, err := xml.MarshalIndent(component, "", "  ")
		if err != nil {
			return fmt.Errorf("marshaling metainfo XML for component %s: %w", component.ID, err)
		}
		outBytes = append(outBytes, xmlBytes...)
		if err = os.WriteFile(metaPath, outBytes, 0644); err != nil {
			return fmt.Errorf("writing metainfo XML %s: %w", metaName, err)
		}
		metainfoPaths = append(metainfoPaths, metaPath)
	}

	fwSig := filepath.Join(tmpDir, "firmware.jcat")
	// sign payload
	if err := f.signMetadata(ctx, fwSig, fwPath); err != nil {
		return fmt.Errorf("signing firmware payload: %w", err)
	}
	// sign each metainfo
	for _, metaPath := range metainfoPaths {
		if err := f.signMetadata(ctx, fwSig, metaPath); err != nil {
			return fmt.Errorf("signing metainfo %s: %w", metaPath, err)
		}
	}

	// Build CAB with firmware file, all metainfo XMLs, and jcat at the end
	cabBaseName := fwFile + ".cab"
	cabPathInCache := filepath.Join(tmpDir, cabBaseName)
	fwupdArgs := []string{"build-cabinet", cabPathInCache, fwPath}
	fwupdArgs = append(fwupdArgs, metainfoPaths...)
	fwupdArgs = append(fwupdArgs, fwSig)
	cmd := exec.CommandContext(ctx, "fwupdtool", fwupdArgs...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		logger.Error("fwupdtool build-cabinet failed", "error", err, "output", string(out))
		return fmt.Errorf("fwupdtool build-cabinet for %s: %w", fwFile, err)
	}

	// Calculate CAB checksums (shared by all components)
	cabSha1, cabSha256, err := calculateChecksums(cabPathInCache)
	if err != nil {
		return fmt.Errorf("failed to calculate CAB checksums: %w", err)
	}

	cabName := cabSha256 + "-" + fwFile + ".cab"
	// Add artifacts section to all components
	for _, component := range components {
		for i := range component.Releases {
			component.Releases[i].Artifacts = []lvfs.Artifact{
				{
					Type:     "binary",
					Location: cabName,
					Checksums: []lvfs.Checksum{
						{Type: "sha1", Value: cabSha1},
						{Type: "sha256", Value: cabSha256},
					},
				},
			}
		}
	}

	// Write CAB to storage backend
	cabFile, err := os.Open(cabPathInCache)
	if err != nil {
		return fmt.Errorf("failed to open CAB file: %w", err)
	}
	defer cabFile.Close()

	if err := f.Storage.Write(ctx, cabName, cabFile); err != nil {
		return fmt.Errorf("failed to write CAB to storage: %w", err)
	}

	return nil
}

func calculateChecksums(filepath string) (sha1Hash, sha256Hash string, err error) {
	file, err := os.Open(filepath)
	if err != nil {
		return "", "", fmt.Errorf("opening %s: %w", filepath, err)
	}
	defer file.Close()

	sha1Hasher := sha1.New()
	sha256Hasher := sha256.New()

	// Use MultiWriter to compute both hashes in one pass
	if _, err := io.Copy(io.MultiWriter(sha1Hasher, sha256Hasher), file); err != nil {
		return "", "", fmt.Errorf("hashing %s: %w", filepath, err)
	}

	sha1Hash = hex.EncodeToString(sha1Hasher.Sum(nil))
	sha256Hash = hex.EncodeToString(sha256Hasher.Sum(nil))

	return sha1Hash, sha256Hash, nil
}

// LoadMetadata loads existing metadata.xml.zst and builds an index of existing firmware
func (f *FirmirrorSyncer) LoadMetadata(ctx context.Context) error {
	metadataKey := "metadata.xml.zst"

	// Check if metadata file exists
	exists, err := f.Storage.Exists(ctx, metadataKey)
	if err != nil {
		return fmt.Errorf("failed to check metadata existence: %w", err)
	}
	if !exists {
		slog.Info("No existing metadata found, starting fresh")
		return nil
	}

	// Read metadata from storage
	reader, err := f.Storage.Read(ctx, metadataKey)
	if err != nil {
		return fmt.Errorf("failed to read metadata file: %w", err)
	}
	defer reader.Close()

	zstReader, err := zstd.NewReader(reader)
	if err != nil {
		return fmt.Errorf("failed to create zstd reader: %w", err)
	}
	defer zstReader.Close()

	// Read and parse XML
	data, err := io.ReadAll(zstReader)
	if err != nil {
		return fmt.Errorf("failed to read metadata file: %w", err)
	}

	var components lvfs.Components
	if err := xml.Unmarshal(data, &components); err != nil {
		return fmt.Errorf("failed to parse metadata XML: %w", err)
	}

	f.existingMetadata = &components

	if components.SchemaVersion != lvfs.MetadataSchemaVersion {
		slog.Warn("Metadata schema version mismatch, forcing full reprocessing",
			"stored_version", components.SchemaVersion,
			"current_version", lvfs.MetadataSchemaVersion,
			"existing_components", len(components.Component))
	} else {
		// Build index of existing firmware files from checksums
		for _, comp := range components.Component {
			for _, release := range comp.Releases {
				for _, checksum := range release.Checksums {
					if checksum.Filename != "" {
						f.existingIndex[checksum.Filename] = true
					}
				}
			}
		}
	}

	slog.Info("Loaded existing metadata",
		"components", len(components.Component),
		"firmware_files", len(f.existingIndex),
		"schema_version", components.SchemaVersion)

	return nil
}

// SaveMetadata saves the combined metadata (existing + accumulated) to metadata.xml.zst
func (f *FirmirrorSyncer) SaveMetadata(ctx context.Context) error {
	ctx = context.WithoutCancel(ctx)
	logger := slog.With("component", "metadata-save")

	if len(f.newComponents) == 0 {
		if !f.Config.CleanupUnreferencedPackages {
			logger.Info("No new component, skipping metadata update")
			return nil
		}
		logger.Info("No new component, skipping metadata update and checking for stale packages")
		return f.removeUnreferencedPackages(ctx, f.existingMetadata)
	}

	componentMap, supersededLocations := mergeComponents(
		f.existingMetadata,
		f.newComponents,
		f.Config.CleanupUnreferencedPackages,
	)
	if !f.Config.CleanupUnreferencedPackages {
		logRetainedSupersededPackages(logger, supersededLocations)
	}

	// Build final components structure (sorted by ID for deterministic output)
	components := &lvfs.Components{
		Origin:        "firmirror",
		SchemaVersion: lvfs.MetadataSchemaVersion,
	}
	keys := slices.Collect(maps.Keys(componentMap))
	slices.Sort(keys)
	for _, k := range keys {
		component := componentMap[k]
		// Ensure each release has a location tag
		for i := range component.Releases {
			release := &component.Releases[i]
			if release.Location == "" && len(release.Artifacts) > 0 && release.Artifacts[0].Location != "" {
				release.Location = release.Artifacts[0].Location
			}
		}
		components.Component = append(components.Component, *component)
	}

	// Marshal metadata to XML
	outBytes := []byte(xml.Header)
	xmlBytes, err := xml.MarshalIndent(components, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal metadata XML: %w", err)
	}
	outBytes = append(outBytes, xmlBytes...)

	// Compress metadata in-memory (avoids disk round-trip)
	var compressedBuf bytes.Buffer
	zstWriter, err := zstd.NewWriter(&compressedBuf)
	if err != nil {
		return fmt.Errorf("failed to create zstd writer: %w", err)
	}
	if _, err := zstWriter.Write(outBytes); err != nil {
		return fmt.Errorf("failed to compress metadata: %w", err)
	}
	if err := zstWriter.Close(); err != nil {
		return fmt.Errorf("failed to finalize zstd compression: %w", err)
	}

	// Write compressed data to temp file for signing (jcat-tool requires a file path)
	compressedPath := filepath.Join(f.Config.CacheDir, "metadata.xml.zst")
	if err := os.WriteFile(compressedPath, compressedBuf.Bytes(), 0644); err != nil {
		return fmt.Errorf("failed to write compressed metadata to temp file: %w", err)
	}
	defer os.Remove(compressedPath)

	// Always create a JCAT file containing checksums. When signing keys are
	// configured, signMetadata also adds a PKCS#7 signature.
	signaturePath := compressedPath + ".jcat"
	if err := f.signMetadata(ctx, signaturePath, compressedPath); err != nil {
		return fmt.Errorf("creating metadata JCAT: %w", err)
	}
	defer os.Remove(signaturePath)

	// Write the JCAT file to storage first (metadata is the commit point).
	sigFile, err := os.Open(signaturePath)
	if err != nil {
		return fmt.Errorf("failed to open metadata JCAT file: %w", err)
	}
	sigData, err := io.ReadAll(sigFile)
	sigFile.Close()
	if err != nil {
		return fmt.Errorf("failed to read metadata JCAT file: %w", err)
	}
	if err := f.Storage.Write(ctx, filepath.Base(signaturePath), bytes.NewReader(sigData)); err != nil {
		return fmt.Errorf("failed to write metadata JCAT to storage: %w", err)
	}

	// Write compressed metadata to storage (commit point — written last)
	if err := f.Storage.Write(ctx, "metadata.xml.zst", bytes.NewReader(compressedBuf.Bytes())); err != nil {
		return fmt.Errorf("failed to write metadata to storage: %w", err)
	}

	// The new metadata is now committed, so unreferenced S3 packages can be
	// removed. A later no-op run retries any transient cleanup failures.
	if err := f.removeUnreferencedPackages(ctx, components); err != nil {
		return fmt.Errorf("metadata saved but failed to remove unreferenced firmware packages: %w", err)
	}

	logger.Info("Metadata saved successfully",
		"total_merged_components", len(componentMap),
		"new_components", len(f.newComponents))

	return nil
}

func firmwareFilenamesByComponent(components []lvfs.Component) map[string]map[string]struct{} {
	filenames := make(map[string]map[string]struct{})
	for _, component := range components {
		for _, release := range component.Releases {
			for _, checksum := range release.Checksums {
				if checksum.Filename != "" {
					if filenames[component.ID] == nil {
						filenames[component.ID] = make(map[string]struct{})
					}
					filenames[component.ID][checksum.Filename] = struct{}{}
				}
			}
		}
	}
	return filenames
}

func releaseContainsFirmware(release lvfs.Release, filenames map[string]struct{}) bool {
	for _, checksum := range release.Checksums {
		if _, ok := filenames[checksum.Filename]; ok && checksum.Filename != "" {
			return true
		}
	}
	return false
}

func mergeComponents(existing *lvfs.Components, incoming []lvfs.Component, replaceSuperseded bool) (map[string]*lvfs.Component, []string) {
	componentMap := make(map[string]*lvfs.Component)
	supersededLocations := make(map[string]struct{})
	newFirmwareFilenames := firmwareFilenamesByComponent(incoming)

	if existing != nil {
		for _, existingComponent := range existing.Component {
			component := existingComponent
			component.Releases = make([]lvfs.Release, 0, len(existingComponent.Releases))
			for _, release := range existingComponent.Releases {
				superseded := releaseContainsFirmware(release, newFirmwareFilenames[component.ID])
				if superseded {
					collectReleaseLocations(release, supersededLocations)
				}
				if !superseded || !replaceSuperseded {
					component.Releases = append(component.Releases, release)
				}
			}
			if len(component.Releases) > 0 {
				componentMap[component.ID] = &component
			}
		}
	}

	for _, incomingComponent := range incoming {
		component := incomingComponent
		if existingComponent, ok := componentMap[component.ID]; ok {
			existingComponent.Releases = append(existingComponent.Releases, component.Releases...)
		} else {
			componentMap[component.ID] = &component
		}
	}

	return componentMap, slices.Sorted(maps.Keys(supersededLocations))
}

func logRetainedSupersededPackages(logger *slog.Logger, locations []string) {
	for _, location := range locations {
		logger.Info("Leaving superseded firmware package untouched because --s3.cleanup was not specified", "location", location)
	}
}

func collectReleaseLocations(release lvfs.Release, locations map[string]struct{}) {
	if release.Location != "" {
		locations[release.Location] = struct{}{}
	}
	for _, artifact := range release.Artifacts {
		if artifact.Location != "" {
			locations[artifact.Location] = struct{}{}
		}
	}
}

func (f *FirmirrorSyncer) removeUnreferencedPackages(ctx context.Context, components *lvfs.Components) error {
	if !f.Config.CleanupUnreferencedPackages {
		return nil
	}

	cleaner, ok := f.Storage.(packageCleaner)
	if !ok || components == nil {
		return nil
	}

	referencedLocations := make(map[string]struct{})
	for _, component := range components.Component {
		for _, release := range component.Releases {
			collectReleaseLocations(release, referencedLocations)
		}
	}

	keys, err := cleaner.List(ctx, "")
	if err != nil {
		return fmt.Errorf("listing stored packages: %w", err)
	}
	slices.Sort(keys)

	logger := slog.With("component", "metadata-save")
	var deleteErrors []error
	for _, key := range keys {
		if !strings.HasSuffix(key, ".cab") {
			continue
		}
		if _, referenced := referencedLocations[key]; referenced {
			continue
		}
		if err := cleaner.Delete(ctx, key); err != nil {
			deleteErrors = append(deleteErrors, fmt.Errorf("deleting %s: %w", key, err))
			continue
		}
		logger.Info("Removed unreferenced firmware package", "location", key)
	}
	return errors.Join(deleteErrors...)
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
