package firmirror

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path"
	"regexp"
	"slices"
	"strings"

	"github.com/premday/firmirror/pkg/lvfs"
)

// fwupd has no notion of a release channel: a client consumes exactly the
// releases listed in the one metadata document its remote points at. A ring is
// therefore its own metadata document, published next to the cabinets so the
// relative <location> of every release keeps resolving against the same pool
// of packages, whichever ring a host is on.
//
// Every ring is two documents: a snapshot, the unfiltered state it was
// promoted with, which is internal to firmirror, and the feed published from
// it, which is what its hosts download. The empty ring is the index: refresh
// maintains its snapshot incrementally, promotions are seeded from it, and its
// feed is what hosts that want the latest firmware the day it is mirrored
// read. Being a ring like the others is what makes the blocklist apply to it:
// those hosts are the ones already running a firmware that turns out to brick
// machines.
//
// Ring names are free-form so a repository can name its rings whatever its
// rollout calls them, and firmirror imposes no order between them: which ring
// is promoted from which is decided by whoever runs promote.
const (
	metadataExtension = ".xml.zst"
	snapshotPrefix    = "snapshot-"
	feedPrefix        = "metadata-"

	// indexSnapshotKey is the index: every firmware mirrored so far, and the
	// state every published document is derived from.
	indexSnapshotKey = "snapshot" + metadataExtension
)

// A ring name has to be safe as both an object key segment and a URL segment,
// and must not start with a dot so it cannot name a hidden or relative path in
// a local repository.
var ringNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

func validateRingName(ring string) error {
	if ring == "" {
		return errors.New("ring name is empty")
	}
	if !ringNamePattern.MatchString(ring) {
		return fmt.Errorf("ring name %q must start with a letter or digit and only contain letters, digits, dots, dashes and underscores", ring)
	}
	return nil
}

// feedKey is the document fwupd clients on the ring download.
func feedKey(ring string) string {
	if ring == "" {
		return IndexKey
	}
	return feedPrefix + ring + metadataExtension
}

// snapshotKey is the unfiltered state a ring was promoted with. It is internal
// to firmirror: clients never fetch it, and it carries no signature.
func snapshotKey(ring string) string {
	if ring == "" {
		return indexSnapshotKey
	}
	return snapshotPrefix + ring + metadataExtension
}

// ringFromSnapshotKey is the inverse of snapshotKey, rejecting keys that are
// not a ring snapshot.
func ringFromSnapshotKey(key string) (string, bool) {
	return ringFromKey(key, snapshotPrefix)
}

// ringFromFeedKey is the inverse of feedKey, rejecting keys that are not a
// ring feed.
func ringFromFeedKey(key string) (string, bool) {
	return ringFromKey(key, feedPrefix)
}

func ringFromKey(key, prefix string) (string, bool) {
	if !strings.HasPrefix(key, prefix) || !strings.HasSuffix(key, metadataExtension) {
		return "", false
	}
	ring := strings.TrimSuffix(strings.TrimPrefix(key, prefix), metadataExtension)
	if validateRingName(ring) != nil {
		return "", false
	}
	return ring, true
}

// ringLabel names the empty ring in logs, where "" would read as missing.
func ringLabel(ring string) string {
	if ring == "" {
		return "(index)"
	}
	return ring
}

// blockedRelease reports whether a release is blocklisted. Both the vendor
// firmware filename from the catalog and the cabinet name fwupd reports as the
// release location are accepted, so an operator can paste whichever one the
// failure gave them.
func blockedRelease(release lvfs.Release, blocked map[string]struct{}) bool {
	candidates := make([]string, 0, len(release.Checksums)+len(release.Artifacts)+1)
	for _, checksum := range release.Checksums {
		if checksum.Filename != "" {
			candidates = append(candidates, checksum.Filename)
		}
	}
	locations := make(map[string]struct{})
	collectReleaseLocations(release, locations)
	for location := range locations {
		candidates = append(candidates, location, path.Base(location))
	}

	for _, candidate := range candidates {
		if _, ok := blocked[candidate]; ok {
			return true
		}
	}
	return false
}

// applyBlocklist returns the components without the blocklisted releases, and
// how many releases that removed. The input is left untouched: the blocklist
// only ever applies to what is published, so removing an entry brings a
// release back without re-mirroring or re-promoting anything.
func applyBlocklist(components *lvfs.Components, blocklist []string) (*lvfs.Components, int) {
	if components == nil {
		return nil, 0
	}

	blocked := make(map[string]struct{}, len(blocklist))
	for _, entry := range blocklist {
		if entry = strings.TrimSpace(entry); entry != "" {
			blocked[entry] = struct{}{}
		}
	}
	if len(blocked) == 0 {
		return components, 0
	}

	filtered := &lvfs.Components{
		Origin:        components.Origin,
		SchemaVersion: components.SchemaVersion,
	}
	dropped := 0
	for _, component := range components.Component {
		kept := make([]lvfs.Release, 0, len(component.Releases))
		for _, release := range component.Releases {
			if blockedRelease(release, blocked) {
				dropped++
				continue
			}
			kept = append(kept, release)
		}

		// A component with every release blocked would advertise a device
		// with nothing to install, so drop it rather than publish it empty.
		if len(kept) == 0 {
			continue
		}
		component.Releases = kept
		filtered.Component = append(filtered.Component, component)
	}
	return filtered, dropped
}

// ringFeed derives what clients on a ring download from the state it holds,
// and reports how many releases each filter removed.
//
// A package a vendor rebuilt is kept next to its rebuild in the state, so that
// mirroring firmware never leaves a package referenced by nothing; publishing
// both would leave fwupd to pick between a cabinet and the one that replaced
// it, with nothing saying which. The superseded release is therefore withheld
// from the feed and reclaimed later, by s3-cleanup.
func (f *FirmirrorSyncer) ringFeed(state *lvfs.Components) (feed *lvfs.Components, superseded, blocked int) {
	pruned, _ := pruneSupersededReleases(state)
	feed, blocked = applyBlocklist(pruned, f.Config.Blocklist)
	return feed, countReleases(state) - countReleases(pruned), blocked
}

// publishFeed writes what clients on the ring download, derived from the state
// it holds. Nothing is uploaded when the document would not change, so a run
// that mirrored no new firmware leaves the feeds alone.
func (f *FirmirrorSyncer) publishFeed(ctx context.Context, ring string, state *lvfs.Components) (bool, error) {
	key := feedKey(ring)
	feed, superseded, blocked := f.ringFeed(state)
	compressed, err := encodeMetadata(feed)
	if err != nil {
		return false, fmt.Errorf("encoding the %s feed: %w", ringLabel(ring), err)
	}

	logger := slog.With("component", "feed", "ring", ringLabel(ring), "key", key)
	if f.storedMetadataMatches(ctx, key, compressed) {
		if err := f.writeMetadataSignature(ctx, key, compressed); err != nil {
			return false, fmt.Errorf("refreshing the %s feed signature: %w", ringLabel(ring), err)
		}
		logger.Info("Feed already up to date",
			"components", len(feed.Component),
			"releases", countReleases(feed),
			"superseded_releases", superseded,
			"blocked_releases", blocked)
		return false, nil
	}

	if err := f.writeSignedMetadata(ctx, key, compressed); err != nil {
		return false, fmt.Errorf("publishing the %s feed: %w", ringLabel(ring), err)
	}
	logger.Info("Published feed",
		"components", len(feed.Component),
		"releases", countReleases(feed),
		"superseded_releases", superseded,
		"blocked_releases", blocked)
	return true, nil
}

// listRings returns every ring the repository holds a document for, the index
// first. Rings are discovered from storage rather than configured, so
// promoting into a new one needs no change to the refresh job.
//
// Both documents of a ring are looked for, not just its snapshot: a feed whose
// snapshot is gone is still what its hosts download, and the only record left
// of the packages they need. A ring missing from this list is a ring whose
// feed nothing republishes and whose packages a cleanup would reclaim.
func (f *FirmirrorSyncer) listRings(ctx context.Context) ([]string, error) {
	rings := []string{""}

	storageLister, ok := f.Storage.(lister)
	if !ok {
		return rings, nil
	}
	for _, documents := range []struct {
		prefix string
		ring   func(string) (string, bool)
	}{
		{snapshotPrefix, ringFromSnapshotKey},
		{feedPrefix, ringFromFeedKey},
	} {
		objects, err := storageLister.List(ctx, documents.prefix)
		if err != nil {
			return nil, fmt.Errorf("listing the %s* documents: %w", documents.prefix, err)
		}
		for _, object := range objects {
			if ring, ok := documents.ring(object.Key); ok {
				rings = append(rings, ring)
			}
		}
	}

	slices.Sort(rings)
	return slices.Compact(rings), nil
}

// readRingState returns the unfiltered state of a ring, together with the key
// it was read from.
//
// That state is the snapshot. A ring left without one, because an operator
// removed it or because the repository predates snapshots, falls back to the
// feed it is serving: filtered, but the only record of what it was promoted
// with, and better than a ring that can neither be republished nor promoted
// out of.
func (f *FirmirrorSyncer) readRingState(ctx context.Context, ring string) (*lvfs.Components, string, bool, error) {
	for _, key := range []string{snapshotKey(ring), feedKey(ring)} {
		state, found, err := f.readComponents(ctx, key)
		if err != nil {
			return nil, "", false, err
		}
		if found {
			return state, key, true, nil
		}
	}
	return nil, "", false, nil
}

// PublishFeeds refreshes the feed of every ring from the state it holds, so a
// blocklist change takes effect on the next run without promoting anything.
//
// It returns every package location those states reference, which is what a
// cleanup has to keep. The documents have just been read, so an inventory does
// not have to download them all again, and a run that failed to read one
// returns no locations at all rather than a set that would have a cleanup
// reclaim the packages of the ring it could not read.
func (f *FirmirrorSyncer) PublishFeeds(ctx context.Context) (map[string]struct{}, error) {
	rings, err := f.listRings(ctx)
	if err != nil {
		return nil, err
	}

	locations := make(map[string]struct{})
	var errs []error
	for _, ring := range rings {
		state, key, found, err := f.readRingState(ctx, ring)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if !found {
			// Nothing has been mirrored or promoted into it yet, or it was
			// removed under us; either way the next run will not list it.
			continue
		}
		collectComponentLocations(state, locations)

		if key != snapshotKey(ring) {
			// Record what the ring is serving as its state before publishing
			// over it, so this is the last run that has to fall back to a
			// feed.
			slog.Warn("Ring has no snapshot, recording the state it is serving as one",
				"component", "feed", "ring", ringLabel(ring), "read_from", key)
			compressed, err := encodeMetadata(state)
			if err != nil {
				errs = append(errs, fmt.Errorf("encoding the %s snapshot: %w", ringLabel(ring), err))
				continue
			}
			if err := f.Storage.Write(ctx, snapshotKey(ring), bytes.NewReader(compressed)); err != nil {
				errs = append(errs, fmt.Errorf("failed to write the %s snapshot to storage: %w", ringLabel(ring), err))
				continue
			}
		}

		if _, err := f.publishFeed(ctx, ring, state); err != nil {
			errs = append(errs, err)
		}
	}
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	return locations, nil
}

// Promote copies the state of one ring into another and publishes the target
// feed from it. The source is a snapshot rather than the live index, so a ring
// can be promoted from a state that was validated days ago while newer
// firmware keeps flowing into the index.
func (f *FirmirrorSyncer) Promote(ctx context.Context, from, to string) error {
	if err := validateRingName(to); err != nil {
		return fmt.Errorf("invalid target ring: %w", err)
	}
	if from != "" {
		if err := validateRingName(from); err != nil {
			return fmt.Errorf("invalid source ring: %w", err)
		}
	}
	if from == to {
		return fmt.Errorf("cannot promote ring %q into itself", to)
	}

	source, _, found, err := f.readRingState(ctx, from)
	if err != nil {
		return err
	}
	if !found {
		if from == "" {
			return fmt.Errorf("%s does not exist: no firmware has been mirrored yet", snapshotKey(from))
		}
		return fmt.Errorf("%s does not exist: nothing has been promoted into ring %q yet", snapshotKey(from), from)
	}

	// Report what the promotion changes for the hosts on the target ring,
	// which is the whole point of running it.
	previousReleases := 0
	if previous, found, err := f.readComponents(ctx, feedKey(to)); err != nil {
		return err
	} else if found {
		previousReleases = countReleases(previous)
	}

	// Commit the state the ring is promoted with before publishing anything
	// from it. Feeds are derived from the snapshots on every run, so a feed
	// published from a state that was never recorded would be rolled back,
	// silently, by the next refresh.
	compressed, err := encodeMetadata(source)
	if err != nil {
		return fmt.Errorf("encoding the %s snapshot: %w", to, err)
	}
	if err := f.Storage.Write(ctx, snapshotKey(to), bytes.NewReader(compressed)); err != nil {
		return fmt.Errorf("failed to write the %s snapshot to storage: %w", to, err)
	}

	// Publish every ring, not just the target: the blocklist applies to all
	// of them, so a ring whose feed predates the current blocklist would
	// otherwise keep serving a release that has since been withheld.
	if _, err := f.PublishFeeds(ctx); err != nil {
		return err
	}

	feed, _, _ := f.ringFeed(source)
	slog.Info("Promoted ring",
		"component", "promote",
		"from", ringLabel(from),
		"to", to,
		"releases_before", previousReleases,
		"releases_after", countReleases(feed))
	return nil
}
