package firmirror

import (
	"bytes"
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
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

	"github.com/premday/firmirror/pkg/lvfs"
)

type FirmirrorConfig struct {
	CacheDir       string        // Local cache directory for temporary work
	Certificate    string        // Path to certificate file for signing metadata (.pem or .crt)
	PrivateKey     string        // Path to private key file for signing metadata (.pem or .key)
	MaxConcurrency int           // Maximum number of firmware entries processed concurrently (default 1)
	Blocklist      []string      // Firmware filenames or CAB names withheld from the published ring feeds
	MinPackageAge  time.Duration // How long a package nothing references is kept before a cleanup may reclaim it
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
	cleanStaleWorkDirs(f.Config.CacheDir)

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

// LoadMetadata loads the index and builds an index of existing firmware
func (f *FirmirrorSyncer) LoadMetadata(ctx context.Context) error {
	components, key, found, err := f.readRingState(ctx, "")
	if err != nil {
		return err
	}
	if !found {
		slog.Info("No existing metadata found, starting fresh")
		return nil
	}
	if key != snapshotKey("") {
		// A repository written before the index had a snapshot of its own, or
		// one whose snapshot was removed: the published feed is the only
		// state left to continue from, blocklisted releases excepted, which
		// this run mirrors again.
		slog.Warn("No index snapshot, continuing from the published index feed", "key", key)
	}

	if components.SchemaVersion != lvfs.MetadataSchemaVersion {
		// Drop the components too, not just the index: they are built
		// differently now, and nothing would ever supersede the stale ones.
		slog.Warn("Metadata schema version mismatch, forcing full reprocessing",
			"stored_version", components.SchemaVersion,
			"current_version", lvfs.MetadataSchemaVersion,
			"discarded_components", len(components.Component))
	} else {
		f.existingMetadata = components

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

// SaveMetadata commits everything this run mirrored, on top of what the index
// already held, and publishes the ring feeds derived from it.
//
// The context is the caller's: a run ended by a signal still owes its
// repository this commit, which is why refresh hands over a context that
// outlives the signal, but one whose repository lock is gone must stop here
// rather than write documents another process may be writing too.
func (f *FirmirrorSyncer) SaveMetadata(ctx context.Context) error {
	logger := slog.With("component", "metadata-save")

	if len(f.newComponents) == 0 {
		// The index is unchanged, but the ring feeds are still republished:
		// that is how a blocklist change takes effect, and it repairs a feed
		// that a previous run failed to write.
		logger.Info("No new component, skipping the index update and refreshing the ring feeds")
		referenced, err := f.PublishFeeds(ctx)
		if err != nil {
			return fmt.Errorf("failed to publish the ring feeds: %w", err)
		}
		f.ReportPackages(ctx, referenced)
		return nil
	}

	componentMap, supersededLocations := mergeComponents(f.existingMetadata, f.newComponents)
	logSupersededPackages(logger, supersededLocations)

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

	compressed, err := encodeMetadata(components)
	if err != nil {
		return err
	}
	// The index is the state of a ring like any other: internal, unsigned,
	// and reaching clients only through the feed published from it, which is
	// where the blocklist applies.
	if err := f.Storage.Write(ctx, snapshotKey(""), bytes.NewReader(compressed)); err != nil {
		return fmt.Errorf("failed to write %s to storage: %w", snapshotKey(""), err)
	}

	// The index is now committed, so every ring feed can be published from
	// the state its ring holds, and the stored packages reconciled against
	// what those states reference. A later no-op run retries transient
	// failures.
	referenced, err := f.PublishFeeds(ctx)
	if err != nil {
		return fmt.Errorf("index saved but failed to publish the ring feeds: %w", err)
	}

	f.ReportPackages(ctx, referenced)

	logger.Info("Metadata saved successfully",
		"total_merged_components", len(componentMap),
		"new_components", len(f.newComponents))

	return nil
}

// componentKey identifies a component by its ID and its GUIDs. One set of GUIDs
// covers all of a component's releases, so a package covering other platforms
// must not merge its releases in: fwupd would reject the cabinet it downloads
// with "No supported devices found".
func componentKey(component lvfs.Component) string {
	guids := make([]string, 0, len(component.Provides))
	for _, provide := range component.Provides {
		guids = append(guids, provide.Text)
	}
	slices.Sort(guids)
	return component.ID + "\n" + strings.Join(slices.Compact(guids), ",")
}

func firmwareFilenamesByComponent(components []lvfs.Component) map[string]map[string]struct{} {
	filenames := make(map[string]map[string]struct{})
	for _, component := range components {
		key := componentKey(component)
		for _, release := range component.Releases {
			for _, checksum := range release.Checksums {
				if checksum.Filename != "" {
					if filenames[key] == nil {
						filenames[key] = make(map[string]struct{})
					}
					filenames[key][checksum.Filename] = struct{}{}
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

// mergeComponents folds the components of this run into the stored ones. A
// release whose package the vendor rebuilt is kept next to its rebuild rather
// than dropped: dropping it would leave its package referenced by nothing, and
// reclaiming packages is s3-cleanup's job, not a side effect of mirroring
// firmware. The locations of the superseded releases are returned so the run
// can report what a cleanup would reclaim.
func mergeComponents(existing *lvfs.Components, incoming []lvfs.Component) (map[string]*lvfs.Component, []string) {
	componentMap := make(map[string]*lvfs.Component)
	supersededLocations := make(map[string]struct{})
	newFirmwareFilenames := firmwareFilenamesByComponent(incoming)

	if existing != nil {
		for _, existingComponent := range existing.Component {
			component := existingComponent
			key := componentKey(component)
			component.Releases = make([]lvfs.Release, 0, len(existingComponent.Releases))
			for _, release := range existingComponent.Releases {
				if releaseContainsFirmware(release, newFirmwareFilenames[key]) {
					collectReleaseLocations(release, supersededLocations)
				}
				component.Releases = append(component.Releases, release)
			}
			if len(component.Releases) > 0 {
				componentMap[key] = &component
			}
		}
	}

	for _, incomingComponent := range incoming {
		component := incomingComponent
		key := componentKey(component)
		if existingComponent, ok := componentMap[key]; ok {
			existingComponent.Releases = append(existingComponent.Releases, component.Releases...)
		} else {
			componentMap[key] = &component
		}
	}

	return componentMap, slices.Sorted(maps.Keys(supersededLocations))
}

func logSupersededPackages(logger *slog.Logger, locations []string) {
	for _, location := range locations {
		logger.Info("Firmware package superseded by a rebuild, kept until s3-cleanup runs", "location", location)
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
