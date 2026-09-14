package firmirror

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/premday/firmirror/pkg/lvfs"
)

// pruneSupersededReleases drops the releases that a later release of the same
// component has already rebuilt. refresh appends a rebuilt package next to the
// one it replaces rather than dropping it, because that would orphan a package
// on a run whose only job is to mirror firmware; this is where the pair is
// resolved, keeping the newer of the two. The locations the pruned releases
// held are returned for logging: whether they can actually be deleted depends
// on what the rest of the repository still points at.
func pruneSupersededReleases(components *lvfs.Components) (*lvfs.Components, []string) {
	if components == nil {
		return nil, nil
	}

	pruned := &lvfs.Components{
		Origin:        components.Origin,
		SchemaVersion: components.SchemaVersion,
	}
	supersededLocations := make(map[string]struct{})
	for _, component := range components.Component {
		// Walk backwards: of two releases carrying the same vendor firmware,
		// the later one is the rebuild that refresh appended.
		rebuilt := make(map[string]struct{}, len(component.Releases))
		kept := make([]lvfs.Release, 0, len(component.Releases))
		for i := len(component.Releases) - 1; i >= 0; i-- {
			release := component.Releases[i]
			if releaseContainsFirmware(release, rebuilt) {
				collectReleaseLocations(release, supersededLocations)
				continue
			}
			for _, checksum := range release.Checksums {
				if checksum.Filename != "" {
					rebuilt[checksum.Filename] = struct{}{}
				}
			}
			kept = append(kept, release)
		}
		slices.Reverse(kept)
		component.Releases = kept
		pruned.Component = append(pruned.Component, component)
	}
	return pruned, slices.Sorted(maps.Keys(supersededLocations))
}

// collectComponentLocations adds every package location the components point
// at to locations.
func collectComponentLocations(components *lvfs.Components, locations map[string]struct{}) {
	if components == nil {
		return
	}
	for _, component := range components.Component {
		for _, release := range component.Releases {
			collectReleaseLocations(release, locations)
		}
	}
}

// packageInventory is what any metadata document points at, against what
// storage actually holds.
type packageInventory struct {
	referenced  map[string]struct{}
	missing     []string
	reclaimable []string
	tooRecent   []string
}

// takeInventory reconciles the packages the metadata references with the
// objects in storage. The locations are the ones PublishFeeds collected from
// the ring states it read, so they cover every document in the repository and
// none of this depends on what the caller happens to hold in memory.
//
// A package nothing references is only reclaimable once it has sat there for
// MinPackageAge. A refresh uploads every cabinet as it builds it and publishes
// the metadata naming them only when the whole run ends, so for the length of
// a run its cabinets are in storage and named by no document yet. Deleting
// those would take away the packages that run is about to publish, and the
// index it then commits would point at objects that are gone. Anything younger
// than the threshold is therefore left for a later cleanup, by when either the
// run has published it or the run is long over and it really is an orphan.
func (f *FirmirrorSyncer) takeInventory(ctx context.Context, referenced map[string]struct{}) (*packageInventory, error) {
	storageLister, ok := f.Storage.(lister)
	if !ok {
		return nil, errors.New("the storage backend cannot list its objects")
	}

	objects, err := storageLister.List(ctx, "")
	if err != nil {
		return nil, fmt.Errorf("listing stored packages: %w", err)
	}

	stored := make(map[string]struct{}, len(objects))
	for _, object := range objects {
		stored[object.Key] = struct{}{}
	}

	inventory := &packageInventory{referenced: referenced}
	for location := range referenced {
		if _, ok := stored[location]; !ok {
			inventory.missing = append(inventory.missing, location)
		}
	}
	now := time.Now()
	for _, object := range objects {
		if !strings.HasSuffix(object.Key, ".cab") {
			continue
		}
		if _, ok := referenced[object.Key]; ok {
			continue
		}
		if now.Sub(object.ModifiedAt) < f.Config.MinPackageAge {
			inventory.tooRecent = append(inventory.tooRecent, object.Key)
			continue
		}
		inventory.reclaimable = append(inventory.reclaimable, object.Key)
	}
	slices.Sort(inventory.missing)
	slices.Sort(inventory.reclaimable)
	slices.Sort(inventory.tooRecent)
	return inventory, nil
}

// ReportPackages logs how the stored packages line up with the metadata,
// without touching anything. Deleting is left to CleanupPackages, so a run
// that mirrors firmware never removes a package as a side effect.
func (f *FirmirrorSyncer) ReportPackages(ctx context.Context, referenced map[string]struct{}) {
	logger := slog.With("component", "packages")

	inventory, err := f.takeInventory(ctx, referenced)
	if err != nil {
		logger.Warn("Unable to reconcile the stored packages with the metadata", "error", err)
		return
	}

	// A feed pointing at a package that is not there surfaces on the client
	// as a download failure, which is far harder to trace back than a line
	// here.
	for _, location := range inventory.missing {
		logger.Error("Metadata references a firmware package missing from storage", "location", location)
	}

	if len(inventory.reclaimable) > 0 {
		logger.Info("Stored firmware packages no metadata document references, run s3-cleanup to remove them",
			"packages", len(inventory.reclaimable))
		logger.Debug("Reclaimable firmware packages", "locations", inventory.reclaimable)
	}
	if len(inventory.tooRecent) > 0 {
		// Expected while a run is in progress, since it publishes the metadata
		// naming its cabinets only at the end.
		logger.Debug("Firmware packages not referenced yet, too recent to reclaim",
			"packages", len(inventory.tooRecent), "locations", inventory.tooRecent)
	}
}

// CleanupPackages reclaims what mirroring firmware deliberately leaves behind.
// It replaces the releases a vendor rebuild superseded, then deletes the
// cabinets that no metadata document points at any more. Everything any
// document still references is kept, so a package only an older ring serves
// survives.
func (f *FirmirrorSyncer) CleanupPackages(ctx context.Context) error {
	cleaner, ok := f.Storage.(packageCleaner)
	if !ok {
		return errors.New("the storage backend does not support deleting packages")
	}

	logger := slog.With("component", "packages")

	index, _, found, err := f.readRingState(ctx, "")
	if err != nil {
		return err
	}

	// Commit the pruned index before deleting anything. Interrupted in
	// between, that leaves packages nothing references, which the next
	// cleanup reclaims, rather than an index pointing at packages that are
	// already gone.
	if found {
		pruned, supersededLocations := pruneSupersededReleases(index)
		// Gate on what pruning removed rather than on the locations it
		// freed: a superseded release carrying no location at all still has
		// to leave the index, or every later cleanup redoes the same no-op.
		if removed := countReleases(index) - countReleases(pruned); removed > 0 {
			compressed, err := encodeMetadata(pruned)
			if err != nil {
				return err
			}
			if err := f.Storage.Write(ctx, snapshotKey(""), bytes.NewReader(compressed)); err != nil {
				return fmt.Errorf("failed to write %s to storage: %w", snapshotKey(""), err)
			}
			logger.Info("Replaced superseded releases in the index",
				"releases", removed, "reclaimable_packages", len(supersededLocations))
		}
	}

	// Republish from the committed states: the feeds stop advertising what
	// was just pruned, and the packages to keep are the ones the documents
	// reference as they are now.
	referenced, err := f.PublishFeeds(ctx)
	if err != nil {
		return err
	}

	inventory, err := f.takeInventory(ctx, referenced)
	if err != nil {
		return err
	}

	for _, location := range inventory.missing {
		logger.Error("Metadata references a firmware package missing from storage", "location", location)
	}

	if len(inventory.tooRecent) > 0 {
		logger.Info("Keeping firmware packages that nothing references yet but are too recent to tell apart from a run in progress",
			"packages", len(inventory.tooRecent), "min_age", f.Config.MinPackageAge)
	}

	var deleteErrors []error
	deleted := 0
	for _, key := range inventory.reclaimable {
		if err := cleaner.Delete(ctx, key); err != nil {
			deleteErrors = append(deleteErrors, fmt.Errorf("deleting %s: %w", key, err))
			continue
		}
		deleted++
		logger.Info("Removed unreferenced firmware package", "location", key)
	}
	logger.Info("Cleanup finished",
		"deleted", deleted,
		"kept", len(inventory.referenced),
		"too_recent", len(inventory.tooRecent),
		"failed", len(deleteErrors))
	return errors.Join(deleteErrors...)
}
