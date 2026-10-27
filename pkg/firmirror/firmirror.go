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
	"io/fs"
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
	"github.com/premday/firmirror/pkg/bkc"
	"github.com/premday/firmirror/pkg/lvfs"
	"github.com/premday/firmirror/pkg/utils"
)

// The published document, and the JCAT clients verify it with.
const (
	metadataKey = "metadata.xml.zst"
	jcatKey     = metadataKey + ".jcat"
)

type FirmirrorConfig struct {
	CacheDir                    string // Local cache directory for temporary work
	Certificate                 string // Path to certificate file for signing metadata (.pem or .crt)
	PrivateKey                  string // Path to private key file for signing metadata (.pem or .key)
	MaxConcurrency              int    // Maximum number of firmware entries processed concurrently (default 1)
	CleanupUnreferencedPackages bool   // Replace rebuilt metadata releases and delete unreferenced stored CAB packages
	FirmwareBaseURL             string // Public URL the packages are served from; locations are published as absolute URLs under it
	Force                       bool   // Publish even if that drops ring tags the stored document has (not the base URL or signing)
	StopSigning                 bool   // Publish without signing keys over a signed JCAT
	DropBaseURL                 bool   // Publish locations without the base URL the stored document has

	// ReloadBKC reads the definitions again just before publishing, as a run can
	// take hours. Nil keeps BKC as given.
	ReloadBKC func(ctx context.Context) (*bkc.File, error)

	// BKC defines what each rollout ring serves, and so the tags of the releases.
	// Nil publishes no ring tag.
	BKC *bkc.File
}

type FirmirrorSyncer struct {
	Config           FirmirrorConfig
	Storage          Storage
	vendors          map[string]Vendor
	existingMetadata *lvfs.Components // Loaded metadata from existing metadata.xml.gz
	existingIndex    map[string]bool  // Index of firmware already in metadata (by filename)
	newComponents    []lvfs.Component // Components accumulated during this run

	facts  storedFacts // Of the document in storage, as of the last read or write
	stored fingerprint // Of the document in storage, as of the last time it was read or written
}

// storedFacts is what a published document says of itself, read before its
// pieces are put together again.
type storedFacts struct {
	schema   int      // The schema version, 0 when it says none
	tags     []string // Component tags in the lvfs namespace, sorted
	absolute bool     // Whether locations are URLs
}

// fingerprint identifies a published document by the digest of its XML, which a
// render is compared with, and by the SHA-256 of the compressed file, which is
// what its JCAT records. The zero value is no document.
type fingerprint struct {
	document [sha256.Size]byte
	file     string
}

func fingerprintOf(document, compressed []byte) fingerprint {
	return fingerprint{document: sha256.Sum256(document), file: fileChecksum(compressed)}
}

// fileChecksum is the SHA-256 of a published file, as its JCAT records it.
func fileChecksum(compressed []byte) string {
	sum := sha256.Sum256(compressed)
	return hex.EncodeToString(sum[:])
}

func (p fingerprint) known() bool {
	return p.file != ""
}

func NewFirmirrorSyncer(config FirmirrorConfig, storage Storage) (*FirmirrorSyncer, error) {
	if err := validateFirmwareBaseURL(config.FirmwareBaseURL); err != nil {
		return nil, err
	}
	if config.BKC != nil {
		if err := config.BKC.Validate(); err != nil {
			return nil, fmt.Errorf("invalid rollout rings: %w", err)
		}
	}

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

	payload, err := os.Stat(fwPath)
	if err != nil {
		return fmt.Errorf("sizing firmware: %w", err)
	}

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
			component.Releases[j].Sizes = []lvfs.Size{{Type: "installed", Bytes: payload.Size()}}
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

	cab, err := os.Stat(cabPathInCache)
	if err != nil {
		return fmt.Errorf("sizing CAB: %w", err)
	}

	cabName := cabSha256 + "-" + fwFile + ".cab"
	// Add artifacts section to all components. The download size is only known
	// now, so it is published but cannot be in the metainfo inside the CAB.
	for _, component := range components {
		for i := range component.Releases {
			component.Releases[i].Sizes = append(component.Releases[i].Sizes, lvfs.Size{Type: "download", Bytes: cab.Size()})
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

// checksumStoredFile is the SHA-256 of the published file, read without holding
// it in memory.
func checksumStoredFile(ctx context.Context, storage Storage) (string, error) {
	reader, err := openStoredFile(ctx, storage)
	if err != nil {
		return "", err
	}
	if reader == nil {
		return "", errors.New("the metadata file is gone")
	}
	defer reader.Close()
	hasher := sha256.New()
	if _, err := io.Copy(hasher, reader); err != nil {
		return "", fmt.Errorf("failed to read metadata file: %w", err)
	}
	return hex.EncodeToString(hasher.Sum(nil)), nil
}

// openStoredFile opens the published file, compressed. It returns nil when
// storage holds none.
func openStoredFile(ctx context.Context, storage Storage) (io.ReadCloser, error) {
	exists, err := storage.Exists(ctx, metadataKey)
	if err != nil {
		return nil, fmt.Errorf("failed to check metadata existence: %w", err)
	}
	if !exists {
		return nil, nil
	}
	reader, err := storage.Read(ctx, metadataKey)
	if err != nil {
		return nil, fmt.Errorf("failed to read metadata file: %w", err)
	}
	return reader, nil
}

// readStoredFile reads the published file, compressed. It reports false when
// storage holds none.
func readStoredFile(ctx context.Context, storage Storage) ([]byte, bool, error) {
	reader, err := openStoredFile(ctx, storage)
	if err != nil || reader == nil {
		return nil, false, err
	}
	defer reader.Close()
	compressed, err := utils.ReadAllLimited(reader, maxDocumentSize, "the metadata file")
	if err != nil {
		return nil, false, err
	}
	return compressed, true, nil
}

func decompress(compressed []byte) ([]byte, error) {
	zstReader, err := zstd.NewReader(bytes.NewReader(compressed))
	if err != nil {
		return nil, fmt.Errorf("failed to create zstd reader: %w", err)
	}
	defer zstReader.Close()
	data, err := utils.ReadAllLimited(zstReader, maxDocumentSize, "the decompressed metadata")
	if err != nil {
		return nil, fmt.Errorf("failed to read metadata file: %w", err)
	}
	return data, nil
}

// maxDocumentSize caps the published file, compressed and decompressed each. The
// mirror of both vendors is some hundreds of megabytes of XML; the cap only
// stops what is not a document from filling the memory of a pod.
const maxDocumentSize int64 = 1 << 30

type storedMetadata struct {
	components  *lvfs.Components
	fingerprint fingerprint
	facts       storedFacts
}

// readStored reads and parses the published document, or returns nil when
// storage holds none.
func (f *FirmirrorSyncer) readStored(ctx context.Context) (*storedMetadata, error) {
	compressed, exists, err := readStoredFile(ctx, f.Storage)
	if err != nil || !exists {
		return nil, err
	}
	return parseStored(compressed)
}

// parseStored parses the published file, whose components are still the pieces
// they were published in (see lvfs.GroupByTags), and fingerprints it. The tags
// are not read back onto the releases: the definitions tag them on every publish.
func parseStored(compressed []byte) (*storedMetadata, error) {
	components, data, err := decodeStored(compressed)
	if err != nil {
		return nil, err
	}
	stored := &storedMetadata{components: components, fingerprint: fingerprintOf(data, compressed), facts: storedFacts{schema: components.SchemaVersion}}
	tags := make(map[string]struct{})
	for _, component := range components.Component {
		if component.Tags != nil {
			for _, tag := range component.Tags.Tag {
				if tag.Namespace == lvfs.TagNamespace {
					tags[tag.Value] = struct{}{}
				}
			}
		}
		for _, release := range component.Releases {
			stored.facts.absolute = stored.facts.absolute || isURL(release.Location) ||
				slices.ContainsFunc(release.Artifacts, func(artifact lvfs.Artifact) bool { return isURL(artifact.Location) })
		}
	}
	stored.facts.tags = slices.Sorted(maps.Keys(tags))
	return stored, nil
}

// decodeStored decompresses and parses the published file, and returns the XML
// too.
func decodeStored(compressed []byte) (*lvfs.Components, []byte, error) {
	data, err := decompress(compressed)
	if err != nil {
		return nil, nil, err
	}
	var components lvfs.Components
	if err := xml.Unmarshal(data, &components); err != nil {
		return nil, nil, fmt.Errorf("failed to parse metadata XML: %w", err)
	}
	return &components, data, nil
}

// untagging refuses to publish without a ring tag the stored document has, as
// the hosts on that ring would stop matching releases. Force overrides it.
func (f *FirmirrorSyncer) untagging() error {
	if f.Config.Force {
		return nil
	}
	possible := f.possibleTags()
	dropped := slices.DeleteFunc(slices.Clone(f.facts.tags), func(tag string) bool {
		_, kept := slices.BinarySearch(possible, tag)
		return kept
	})
	if len(dropped) == 0 {
		return nil
	}
	shown := dropped[:min(len(dropped), 5)]
	hint := ""
	if f.Config.BKC != nil && slices.ContainsFunc(f.Config.BKC.Rings, func(ring string) bool {
		return slices.Contains(dropped, f.Config.BKC.Tag(ring)) && f.Config.BKC.HeldOnly(ring)
	}) {
		hint = ", where a ring whose entries are all held serves nothing"
	}
	return fmt.Errorf("the stored metadata has ring tags that would no longer be published (%s, %d in all), which takes the hosts on those rings off them: "+
		"check the rollout ring definitions (--bkc.file)%s, or force it if that is the point", strings.Join(shown, ", "), len(dropped), hint)
}

// noRingTag refuses a built document with no ring tag at all over a stored one
// that has some: that means definitions for another mirror, not a ring whose
// versions were lost. A stored document discarded for its schema version is exempt,
// as the re-mirror is only partly tagged until it finishes. Force overrides it.
func (f *FirmirrorSyncer) noRingTag(published []string) error {
	if f.Config.Force || f.existingMetadata == nil || len(f.facts.tags) == 0 || len(published) > 0 {
		return nil
	}
	return errors.New("the document to publish has no ring tag at all, although the stored metadata has some: the definitions name nothing the mirror has, " +
		"which takes every host off its ring: check the rollout ring definitions (--bkc.file), or force it if that is the point")
}

// checkTags is what a built document must pass before it is published: no stored
// ring tag dropped, and some ring tag when the stored document has some.
func (f *FirmirrorSyncer) checkTags(published []string) error {
	if err := f.untagging(); err != nil {
		return err
	}
	return f.noRingTag(published)
}

// possibleTags are the tags the definitions can publish, sorted: those of the
// rings some entry that is not held names a version for. A ring whose versions
// the mirror lost still counts, as its hosts stay where they are.
func (f *FirmirrorSyncer) possibleTags() []string {
	if f.Config.BKC == nil {
		return nil
	}
	var tags []string
	for _, ring := range f.Config.BKC.Rings {
		if f.Config.BKC.Serves(ring) {
			tags = append(tags, f.Config.BKC.Tag(ring))
		}
	}
	slices.Sort(tags)
	return tags
}

// signatureKept refuses to publish without signing keys over a signed JCAT, which
// clients that require signed metadata would reject. A stored JCAT that is
// certainly malformed is replaced; one that could not be read may be signed, so
// that is refused too. StopSigning overrides it, Force does not.
func (f *FirmirrorSyncer) signatureKept(jcat *storedJCAT, readErr error) error {
	var problem error
	switch {
	case f.signing():
		return nil
	case errors.Is(readErr, errMalformedJCAT):
		slog.Warn("The stored JCAT is not one, so it is replaced", "error", readErr)
	case readErr != nil:
		problem = fmt.Errorf("could not check whether the stored JCAT is signed, which publishing without signing keys would replace "+
			"(delete it if it is cut short, and the run will replace it): %w", readErr)
	case jcat != nil && jcat.isSigned():
		problem = errors.New("the stored JCAT is signed, and without signing keys it would be replaced by one holding only checksums, " +
			"which clients that require signed metadata reject: pass --sign.certificate and --sign.private-key, or --stop-signing to stop signing")
	}
	if problem != nil && f.Config.StopSigning {
		slog.Warn("Publishing without signing keys over a signed JCAT, as asked to", "reason", problem)
		return nil
	}
	return problem
}

// Preflight fails, before anything is mirrored, on what the final publish would
// refuse: a stored ring tag that no ring of the definitions can publish, a
// dropped base URL, and no signing keys over a signed JCAT. The publish repeats
// these and also checks that the built document has some ring tag, which only it
// can tell.
func (f *FirmirrorSyncer) Preflight(ctx context.Context) error {
	if err := f.untagging(); err != nil {
		return err
	}
	if err := f.baseURLDropped(); err != nil {
		return err
	}
	if !f.signing() && f.stored.known() {
		jcat, err := f.readStoredJCATWithRetry(ctx)
		return f.signatureKept(jcat, err)
	}
	return nil
}

// reloadDefinitions swaps in the rollout ring definitions read again, within
// bkc.ReadTimeout, and returns the ones it replaced, which are nil when the run
// had none, and whether it did. When the new ones cannot be read the old ones are
// kept: publishing the run's work matters more than the promotion it may miss.
func (f *FirmirrorSyncer) reloadDefinitions(ctx context.Context) (replaced *bkc.File, ok bool) {
	if f.Config.ReloadBKC == nil {
		return nil, false
	}
	ctx, cancel := context.WithTimeout(ctx, bkc.ReadTimeout)
	defer cancel()
	fresh, err := f.Config.ReloadBKC(ctx)
	if err != nil {
		slog.Warn("Could not read the rollout rings again, publishing with the ones this run started with", "error", err)
		return nil, false
	}
	replaced, f.Config.BKC = f.Config.BKC, fresh
	return replaced, true
}

// LoadMetadata loads existing metadata.xml.zst and builds an index of existing firmware
func (f *FirmirrorSyncer) LoadMetadata(ctx context.Context) error {
	stored, err := f.readStored(ctx)
	if err != nil {
		return err
	}
	if stored == nil {
		slog.Info("No existing metadata found, starting fresh")
		return nil
	}
	components := stored.components
	f.stored = stored.fingerprint
	f.facts = stored.facts

	if components.SchemaVersion != lvfs.MetadataSchemaVersion {
		// Drop the components too, not just the index: they are built
		// differently now, and nothing would ever supersede the stale ones.
		slog.Warn("Metadata schema version mismatch, forcing full reprocessing",
			"stored_version", components.SchemaVersion,
			"current_version", lvfs.MetadataSchemaVersion,
			"discarded_components", len(components.Component))
	} else {
		// Locations go back to the name the package is stored under, whatever
		// base URL an earlier run published them with.
		f.existingMetadata = rewriteLocations(components, bareLocation)

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

// schemaError says that a document is not of the schema version this firmirror
// publishes.
func schemaError(what string, schema int, hint string) error {
	return fmt.Errorf("%s has schema version %d and this firmirror publishes %d: %s", what, schema, lvfs.MetadataSchemaVersion, hint)
}

// noMetadata explains why there is no current document, and is nil when there is
// one.
func (f *FirmirrorSyncer) noMetadata() error {
	switch {
	case f.existingMetadata != nil:
		return nil
	case f.stored.known():
		return schemaError("the stored metadata", f.facts.schema, "run a refresh, which mirrors everything again")
	default:
		return errors.New("there is no metadata: run a refresh first")
	}
}

// SaveMetadata saves the combined metadata (existing + accumulated) to metadata.xml.zst
func (f *FirmirrorSyncer) SaveMetadata(ctx context.Context) error {
	ctx = context.WithoutCancel(ctx)
	logger := slog.With("component", "metadata-save")

	// A run that mirrored nothing and holds no current document has nothing to
	// publish, and must not replace whatever is stored with an empty one.
	if f.existingMetadata == nil && len(f.newComponents) == 0 {
		logger.Info("No new component and no current metadata, skipping metadata update")
		return nil
	}

	replaced, reloaded := f.reloadDefinitions(ctx)
	document, supersededLocations, report := f.buildDocument(f.Config.CleanupUnreferencedPackages)
	// The definitions read again must pass the checks the ones the run started
	// with did; otherwise those are used.
	if reloaded {
		if err := f.checkTags(tagsOf(document)); err != nil {
			logger.Warn("The rollout rings read again would stop publishing a ring tag, so this run publishes with the ones it started with: run again with --force, or fix them, to publish them",
				"error", err)
			f.Config.BKC = replaced
			document, supersededLocations, report = f.buildDocument(f.Config.CleanupUnreferencedPackages)
		}
	}
	// Report only for the definitions finally used.
	if report != nil {
		report.LogTo(slog.With("component", "bkc"))
	}
	if !f.Config.CleanupUnreferencedPackages {
		logRetainedSupersededPackages(logger, supersededLocations)
	}

	outcome, err := f.writeMetadata(ctx, document, false)
	if err != nil {
		return err
	}
	switch outcome {
	case metadataWritten:
		logger.Info("Metadata saved successfully",
			"total_merged_components", len(document.Component),
			"new_components", len(f.newComponents))
	case metadataUnchanged:
		logger.Info("Published metadata is unchanged, skipping metadata update", "components", len(document.Component))
	case metadataUnverified:
		return errors.New("could not check whether the metadata was published meanwhile, so what this run changed is not published: run it again")
	case metadataRepublished, metadataUnchecked:
		// Reclaiming against this run's document would delete the other run's packages.
		return nil
	}

	// The stored metadata is now the one just built, so unreferenced S3 packages
	// can be removed. A later no-op run retries any transient cleanup failures.
	if err := f.removeUnreferencedPackages(ctx, document); err != nil {
		if outcome == metadataWritten {
			return fmt.Errorf("metadata saved but failed to remove unreferenced firmware packages: %w", err)
		}
		return fmt.Errorf("failed to remove unreferenced firmware packages: %w", err)
	}

	return nil
}

// buildDocument merges what is stored with what this run mirrored and returns the
// document to publish, sorted by component for a deterministic output, with the
// releases tagged for the rings the definitions have them serve.
func (f *FirmirrorSyncer) buildDocument(replaceSuperseded bool) (*lvfs.Components, []string, *bkc.Report) {
	componentMap, supersededLocations := mergeComponents(f.existingMetadata, f.newComponents, replaceSuperseded)

	// Build final components structure (sorted by ID for deterministic output)
	document := &lvfs.Components{
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
		// ProcessVendor accumulates components in whatever order its workers
		// finish in, so without this the document differs between two runs that
		// mirrored the same firmware.
		lvfs.SortReleases(component.Releases)
		document.Component = append(document.Component, *component)
	}

	var report *bkc.Report
	if f.Config.BKC != nil {
		report = f.Config.BKC.Apply(document.Component)
	}
	return document, supersededLocations, report
}

// renderMetadata is the XML a document is published as, its components grouped
// by tag set.
func (f *FirmirrorSyncer) renderMetadata(document *lvfs.Components) ([]byte, error) {
	published := rewriteLocations(document, f.publishedLocation)
	published.Component = lvfs.GroupByTags(published.Component)
	xmlBytes, err := xml.MarshalIndent(published, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("failed to marshal metadata XML: %w", err)
	}
	return append([]byte(xml.Header), xmlBytes...), nil
}

func tagsOf(document *lvfs.Components) []string {
	tags := make(map[string]struct{})
	for _, component := range document.Component {
		for _, release := range component.Releases {
			for _, tag := range release.Tags {
				tags[tag] = struct{}{}
			}
		}
	}
	return slices.Sorted(maps.Keys(tags))
}

// retryStorage runs a read of the store that decides whether to publish, again
// twice if it fails: one failure of a store must not lose a run's work. What
// another try cannot change is not retried.
func retryStorage(ctx context.Context, read func() error) error {
	delay := jcatRetryDelay
	for attempt := 0; ; attempt++ {
		err := read()
		if err == nil || errors.Is(err, errMalformedJCAT) || errors.Is(err, fs.ErrPermission) || errors.Is(err, context.Canceled) || attempt == 2 {
			return err
		}
		if sleepErr := utils.Sleep(ctx, delay); sleepErr != nil {
			return errors.Join(err, sleepErr)
		}
		delay *= 2
	}
}

// settled is metadataWritten when the document is to be written, and otherwise
// why there is nothing to write: another run published since this one read the
// metadata, or the document is unchanged and its JCAT matches it. An unchanged
// document with a stale JCAT is written again, as nothing else repairs a write
// that stopped between the JCAT and the metadata. When the store cannot be checked
// the outcome is unverified.
func (f *FirmirrorSyncer) settled(ctx context.Context, jcat *storedJCAT, jcatErr error, unchanged bool) writeOutcome {
	var exists bool
	err := retryStorage(ctx, func() (err error) {
		exists, err = f.Storage.Exists(ctx, metadataKey)
		return err
	})
	if err != nil {
		slog.Warn("Could not check whether the metadata was published again meanwhile", "error", err)
		return unverified(unchanged)
	}
	if !exists {
		slog.Warn("The stored metadata is gone, publishing it again")
		return metadataWritten
	}
	// A JCAT holding the checksum of the file this run read shows nothing was
	// published since, as the JCAT is written before the metadata. Otherwise hash
	// the stored file.
	if jcatErr != nil || jcat == nil || !jcat.hasChecksum(f.stored.file) {
		var current string
		err := retryStorage(ctx, func() (err error) {
			current, err = checksumStoredFile(ctx, f.Storage)
			return err
		})
		switch {
		case err != nil:
			slog.Warn("Could not check whether the metadata was published again meanwhile", "error", err)
			return unverified(unchanged)
		case current != f.stored.file:
			slog.Warn("The metadata was published again while this run was working, leaving it alone")
			return metadataRepublished
		}
	}

	switch {
	case jcatErr != nil && unchanged && !f.signing() && !f.Config.StopSigning && !errors.Is(jcatErr, errMalformedJCAT):
		// The unreadable JCAT may be signed, which a write without keys would
		// replace: leave it for the next run. With keys, or asked to stop signing,
		// the write replaces it.
		slog.Warn("Could not check the stored JCAT, and the metadata is unchanged, so leaving it alone", "error", jcatErr)
		return metadataUnchecked
	case jcatErr != nil:
		slog.Warn("Could not check the stored JCAT, publishing the metadata again", "error", jcatErr)
	case unchanged && f.storedJCATCurrent(jcat):
		return metadataUnchanged
	case unchanged:
		slog.Warn("The stored JCAT does not match the stored metadata, publishing it again")
	}
	return metadataWritten
}

// writeOutcome is what writeMetadata did.
type writeOutcome int

const (
	metadataWritten writeOutcome = iota
	// metadataUnchanged is a document identical to the stored one.
	metadataUnchanged
	// metadataRepublished is a store another run published to since this one read it.
	metadataRepublished
	// metadataUnchecked is an unchanged document that could not be checked against
	// the store, which there is nothing to lose by leaving alone.
	metadataUnchecked
	// metadataUnverified is a changed document that could not be checked against the
	// store, so it was not written.
	metadataUnverified
)

// unverified is the outcome when the store could not be checked.
func unverified(unchanged bool) writeOutcome {
	if unchanged {
		return metadataUnchecked
	}
	return metadataUnverified
}

// writeMetadata compresses, signs and stores the document, unless it is
// unchanged or another run published since. The JCAT goes first and the metadata
// last, as the commit point.
//
// A document identical to the stored one is left alone, unless republish says to
// write it anyway, as a new signing key does not change the document. Signing it
// again changes the JCAT, as a PKCS#7 signature carries its signing time, and
// every client compares the JCAT to decide whether to download the metadata
// again.
func (f *FirmirrorSyncer) writeMetadata(ctx context.Context, document *lvfs.Components, republish bool) (writeOutcome, error) {
	outBytes, err := f.renderMetadata(document)
	if err != nil {
		return metadataWritten, err
	}

	digest := sha256.Sum256(outBytes)
	unchanged := !republish && f.stored.known() && f.stored.document == digest

	// The stored JCAT tells whether the stored metadata is current and whether a
	// publish without keys would replace a signature.
	checkState := f.stored.known() && (unchanged || len(f.newComponents) == 0)
	var jcat *storedJCAT
	var jcatErr error
	if checkState || (!f.signing() && f.stored.known()) {
		jcat, jcatErr = f.readStoredJCATWithRetry(ctx)
	}

	if checkState {
		if outcome := f.settled(ctx, jcat, jcatErr, unchanged); outcome != metadataWritten {
			return outcome, nil
		}
	}

	tags := tagsOf(document)
	if err := f.checkTags(tags); err != nil {
		return metadataWritten, err
	}
	if err := f.baseURLDropped(); err != nil {
		return metadataWritten, err
	}
	if f.stored.known() {
		if err := f.signatureKept(jcat, jcatErr); err != nil {
			return metadataWritten, err
		}
	}

	// Compress metadata in-memory (avoids disk round-trip)
	var compressedBuf bytes.Buffer
	zstWriter, err := zstd.NewWriter(&compressedBuf)
	if err != nil {
		return metadataWritten, fmt.Errorf("failed to create zstd writer: %w", err)
	}
	if _, err := zstWriter.Write(outBytes); err != nil {
		return metadataWritten, fmt.Errorf("failed to compress metadata: %w", err)
	}
	if err := zstWriter.Close(); err != nil {
		return metadataWritten, fmt.Errorf("failed to finalize zstd compression: %w", err)
	}

	// Write compressed data to temp file for signing (jcat-tool requires a file path)
	compressedPath := filepath.Join(f.Config.CacheDir, metadataKey)
	if err := os.WriteFile(compressedPath, compressedBuf.Bytes(), 0644); err != nil {
		return metadataWritten, fmt.Errorf("failed to write compressed metadata to temp file: %w", err)
	}
	defer os.Remove(compressedPath)

	// Always create a JCAT file containing checksums. When signing keys are
	// configured, signMetadata also adds a PKCS#7 signature.
	signaturePath := compressedPath + ".jcat"
	// jcat-tool adds to a JCAT that exists, so one left by a run that failed or was
	// killed would keep its stale checksum, which makes clients reject the result.
	os.Remove(signaturePath)
	defer os.Remove(signaturePath)
	if err := f.signMetadata(ctx, signaturePath, compressedPath); err != nil {
		return metadataWritten, fmt.Errorf("creating metadata JCAT: %w", err)
	}

	// Write the JCAT file to storage first (metadata is the commit point).
	sigData, err := os.ReadFile(signaturePath)
	if err != nil {
		return metadataWritten, fmt.Errorf("failed to read metadata JCAT file: %w", err)
	}
	if err := f.Storage.Write(ctx, jcatKey, bytes.NewReader(sigData)); err != nil {
		return metadataWritten, fmt.Errorf("failed to write metadata JCAT to storage: %w", err)
	}

	// Write compressed metadata to storage (commit point — written last)
	if err := f.Storage.Write(ctx, metadataKey, bytes.NewReader(compressedBuf.Bytes())); err != nil {
		return metadataWritten, fmt.Errorf("failed to write metadata to storage: %w", err)
	}

	f.stored = fingerprint{document: digest, file: fileChecksum(compressedBuf.Bytes())}
	f.facts = storedFacts{schema: document.SchemaVersion, tags: tags, absolute: f.Config.FirmwareBaseURL != ""}
	return metadataWritten, nil
}

func firmwareFilenamesByComponent(components []lvfs.Component) map[string]map[string]struct{} {
	filenames := make(map[string]map[string]struct{})
	for _, component := range components {
		key := lvfs.ComponentKey(component)
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

func mergeComponents(existing *lvfs.Components, incoming []lvfs.Component, replaceSuperseded bool) (map[string]*lvfs.Component, []string) {
	componentMap := make(map[string]*lvfs.Component)
	supersededLocations := make(map[string]struct{})
	newFirmwareFilenames := firmwareFilenamesByComponent(incoming)

	if existing != nil {
		// LVFS publishes one component per firmware, so a document can hold several
		// with the same key. Pool them instead of letting the last one win.
		for _, existingComponent := range lvfs.PoolByKey(existing.Component) {
			component := existingComponent
			key := lvfs.ComponentKey(component)
			component.Releases = make([]lvfs.Release, 0, len(existingComponent.Releases))
			for _, release := range existingComponent.Releases {
				superseded := releaseContainsFirmware(release, newFirmwareFilenames[key])
				if superseded {
					collectReleaseLocations(release, supersededLocations)
				}
				if !superseded || !replaceSuperseded {
					component.Releases = append(component.Releases, release)
				}
			}
			if len(component.Releases) == 0 {
				continue
			}
			componentMap[key] = &component
		}
	}

	// Pooled too, so that merging never appends into the caller's slices.
	for _, component := range lvfs.PoolByKey(incoming) {
		key := lvfs.ComponentKey(component)
		if existingComponent, ok := componentMap[key]; ok {
			existingComponent.Releases = append(existingComponent.Releases, component.Releases...)
		} else {
			// Building the document tags and sorts the releases in place, and the
			// run still holds these.
			component.Releases = slices.Clone(component.Releases)
			componentMap[key] = &component
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

// signing reports whether the metadata is signed, and not only checksummed.
func (f *FirmirrorSyncer) signing() bool {
	return f.Config.Certificate != "" && f.Config.PrivateKey != ""
}

// signMetadata creates a JCAT file for the given file using jcat-tool.
// The JCAT file contains a SHA256 checksum and a signature if signing keys are provided.
func (f *FirmirrorSyncer) signMetadata(ctx context.Context, sigPath, filePath string) error {
	jcatTool := func(args []string, wd string) error {
		slog.Debug("Running jcat-tool", "args", args)
		cmd := exec.CommandContext(ctx, "jcat-tool", args...)
		ownProcessGroup(cmd)
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

	if f.signing() {
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
