package firmirror

import (
	"context"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/premday/firmirror/pkg/lvfs"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRingKeys(t *testing.T) {
	t.Run("EmptyRingIsTheIndex", func(t *testing.T) {
		assert.Equal(t, "metadata.xml.zst", feedKey(""))
		assert.Equal(t, "snapshot.xml.zst", snapshotKey(""),
			"the index is a ring state like any other: internal, and published through its feed")
	})

	t.Run("NamedRingGetsItsOwnDocuments", func(t *testing.T) {
		assert.Equal(t, "metadata-stable.xml.zst", feedKey("stable"))
		assert.Equal(t, "snapshot-stable.xml.zst", snapshotKey("stable"))
	})

	t.Run("RingKeysRoundTrip", func(t *testing.T) {
		for _, ring := range []string{"beta", "preview", "stable", "2024Q1", "ring.a_b-c"} {
			parsed, ok := ringFromSnapshotKey(snapshotKey(ring))
			assert.True(t, ok, "expected the %q snapshot to round trip", ring)
			assert.Equal(t, ring, parsed)

			parsed, ok = ringFromFeedKey(feedKey(ring))
			assert.True(t, ok, "expected the %q feed to round trip", ring)
			assert.Equal(t, ring, parsed)
		}
	})

	t.Run("IgnoresKeysThatAreNotFeeds", func(t *testing.T) {
		for _, key := range []string{
			"metadata.xml.zst",
			"snapshot.xml.zst",
			"snapshot-stable.xml.zst",
			"metadata-stable.xml.zst.jcat",
			"metadata-.xml.zst",
			"metadata-../escape.xml.zst",
		} {
			_, ok := ringFromFeedKey(key)
			assert.False(t, ok, "expected %q not to be read as a ring feed", key)
		}
	})

	t.Run("IgnoresKeysThatAreNotSnapshots", func(t *testing.T) {
		for _, key := range []string{
			"metadata.xml.zst",
			"metadata-stable.xml.zst",
			"snapshot.xml.zst",
			"snapshot-stable.xml.zst.jcat",
			"abc-firmware.bin.cab",
			"snapshot-.xml.zst",
			"snapshot-../escape.xml.zst",
		} {
			_, ok := ringFromSnapshotKey(key)
			assert.False(t, ok, "expected %q not to be read as a ring snapshot", key)
		}
	})

	t.Run("RejectsUnsafeRingNames", func(t *testing.T) {
		for _, ring := range []string{"", ".", "..", ".hidden", "with/slash", "with space", "-leading"} {
			assert.Error(t, validateRingName(ring), "expected %q to be rejected", ring)
		}
	})
}

func TestApplyBlocklist(t *testing.T) {
	components := testComponents(
		testRelease("1.0.0", "firmware-v1.bin", "aaa-firmware-v1.bin.cab"),
		testRelease("2.0.0", "firmware-v2.bin", "bbb-firmware-v2.bin.cab"),
	)

	t.Run("KeepsEverythingWithoutABlocklist", func(t *testing.T) {
		filtered, dropped := applyBlocklist(components, nil)
		assert.Equal(t, 0, dropped)
		assert.Equal(t, []string{"1.0.0", "2.0.0"}, releaseVersions(filtered))
	})

	t.Run("MatchesTheVendorFirmwareFilename", func(t *testing.T) {
		filtered, dropped := applyBlocklist(components, []string{"firmware-v2.bin"})
		assert.Equal(t, 1, dropped)
		assert.Equal(t, []string{"1.0.0"}, releaseVersions(filtered))
	})

	t.Run("MatchesTheCabinetName", func(t *testing.T) {
		filtered, dropped := applyBlocklist(components, []string{"aaa-firmware-v1.bin.cab"})
		assert.Equal(t, 1, dropped)
		assert.Equal(t, []string{"2.0.0"}, releaseVersions(filtered))
	})

	t.Run("IgnoresBlankEntries", func(t *testing.T) {
		filtered, dropped := applyBlocklist(components, []string{"", "   "})
		assert.Equal(t, 0, dropped)
		assert.Equal(t, []string{"1.0.0", "2.0.0"}, releaseVersions(filtered))
	})

	t.Run("TrimsEntries", func(t *testing.T) {
		filtered, dropped := applyBlocklist(components, []string{"  firmware-v1.bin\n"})
		assert.Equal(t, 1, dropped)
		assert.Equal(t, []string{"2.0.0"}, releaseVersions(filtered))
	})

	t.Run("DropsAComponentLeftWithoutAnyRelease", func(t *testing.T) {
		filtered, dropped := applyBlocklist(components, []string{"firmware-v1.bin", "firmware-v2.bin"})
		assert.Equal(t, 2, dropped)
		assert.Empty(t, filtered.Component)
	})

	t.Run("LeavesTheInputUntouched", func(t *testing.T) {
		_, dropped := applyBlocklist(components, []string{"firmware-v1.bin"})
		require.Equal(t, 1, dropped)
		assert.Equal(t, []string{"1.0.0", "2.0.0"}, releaseVersions(components),
			"the blocklist must only apply to what is published, so unblocking restores the release")
	})

	t.Run("KeepsOtherReleasesOfTheSameComponent", func(t *testing.T) {
		shared := &lvfs.Components{Component: []lvfs.Component{{
			ID: "com.test.shared",
			Releases: []lvfs.Release{
				testRelease("1.0.0", "firmware-v1.bin", "aaa-firmware-v1.bin.cab"),
				testRelease("2.0.0", "firmware-v2.bin", "bbb-firmware-v2.bin.cab"),
			},
		}}}
		filtered, dropped := applyBlocklist(shared, []string{"firmware-v2.bin"})
		assert.Equal(t, 1, dropped)
		require.Len(t, filtered.Component, 1)
		assert.Equal(t, []string{"1.0.0"}, releaseVersions(filtered))
	})
}

func TestPublishFeed(t *testing.T) {
	t.Run("WritesTheFeedAndItsSignature", func(t *testing.T) {
		syncer, tmpDir := createTestSyncer(t)
		snapshot := testComponents(testRelease("1.0.0", "firmware-v1.bin", "aaa-firmware-v1.bin.cab"))

		written, err := syncer.publishFeed(context.Background(), "stable", snapshot)
		require.NoError(t, err)
		assert.True(t, written)
		assert.FileExists(t, filepath.Join(tmpDir, "output", "metadata-stable.xml.zst"))
		assert.FileExists(t, filepath.Join(tmpDir, "output", "metadata-stable.xml.zst.jcat"))
	})

	t.Run("SkipsAnUnchangedFeed", func(t *testing.T) {
		syncer, _ := createTestSyncer(t)
		snapshot := testComponents(testRelease("1.0.0", "firmware-v1.bin", "aaa-firmware-v1.bin.cab"))

		written, err := syncer.publishFeed(context.Background(), "stable", snapshot)
		require.NoError(t, err)
		require.True(t, written)

		written, err = syncer.publishFeed(context.Background(), "stable", snapshot)
		require.NoError(t, err)
		assert.False(t, written, "a feed whose content did not change must not be re-uploaded")
	})

	t.Run("RepairsAnUnchangedFeedsSignature", func(t *testing.T) {
		syncer, tmpDir := createTestSyncer(t)
		snapshot := testComponents(testRelease("1.0.0", "firmware-v1.bin", "aaa-firmware-v1.bin.cab"))

		written, err := syncer.publishFeed(context.Background(), "stable", snapshot)
		require.NoError(t, err)
		require.True(t, written)
		signaturePath := filepath.Join(tmpDir, "output", "metadata-stable.xml.zst.jcat")
		require.NoError(t, os.WriteFile(signaturePath, []byte("stale signature"), 0644))

		written, err = syncer.publishFeed(context.Background(), "stable", snapshot)
		require.NoError(t, err)
		assert.False(t, written, "the metadata itself was unchanged")
		contents, err := os.ReadFile(signaturePath)
		require.NoError(t, err)
		assert.NotContains(t, string(contents), "stale signature",
			"the signature is recreated from the document, not patched")
	})

	t.Run("WithholdsASupersededRelease", func(t *testing.T) {
		syncer, _ := createTestSyncer(t)
		// What refresh leaves behind once a vendor rebuilt a package: both
		// releases, so that mirroring firmware orphans no cabinet.
		state := &lvfs.Components{
			Origin:        "firmirror",
			SchemaVersion: lvfs.MetadataSchemaVersion,
			Component: []lvfs.Component{{
				ID: "com.test.firmware",
				Releases: []lvfs.Release{
					testRelease("1.0.0", "firmware.bin", "old-rebuilt.cab"),
					testRelease("1.0.0", "firmware.bin", "new-rebuilt.cab"),
				},
			}},
		}

		_, err := syncer.publishFeed(context.Background(), "stable", state)
		require.NoError(t, err)

		feed := readTestMetadata(t, syncer, feedKey("stable"))
		require.Len(t, feed.Component, 1)
		require.Len(t, feed.Component[0].Releases, 1,
			"nothing tells fwupd which of two cabinets of the same firmware to install")
		assert.Equal(t, "new-rebuilt.cab", feed.Component[0].Releases[0].Location)
	})

	t.Run("WithholdsBlockedReleases", func(t *testing.T) {
		syncer, _ := createTestSyncer(t)
		syncer.Config.Blocklist = []string{"firmware-v2.bin"}
		snapshot := testComponents(
			testRelease("1.0.0", "firmware-v1.bin", "aaa-firmware-v1.bin.cab"),
			testRelease("2.0.0", "firmware-v2.bin", "bbb-firmware-v2.bin.cab"),
		)

		_, err := syncer.publishFeed(context.Background(), "stable", snapshot)
		require.NoError(t, err)
		assert.Equal(t, []string{"1.0.0"}, releaseVersions(readTestMetadata(t, syncer, "metadata-stable.xml.zst")))
	})
}

func TestPromote(t *testing.T) {
	ctx := context.Background()

	t.Run("RejectsAnInvalidPromotion", func(t *testing.T) {
		syncer, _ := createTestSyncer(t)
		writeTestMetadata(t, syncer, snapshotKey(""), testComponents(testRelease("1.0.0", "fw.bin", "aaa-fw.bin.cab")))

		assert.ErrorContains(t, syncer.Promote(ctx, "", ""), "invalid target ring")
		assert.ErrorContains(t, syncer.Promote(ctx, "", "with/slash"), "invalid target ring")
		assert.ErrorContains(t, syncer.Promote(ctx, "with/slash", "stable"), "invalid source ring")
		assert.ErrorContains(t, syncer.Promote(ctx, "stable", "stable"), "into itself")
	})

	t.Run("RejectsAMissingSource", func(t *testing.T) {
		syncer, _ := createTestSyncer(t)
		writeTestMetadata(t, syncer, snapshotKey(""), testComponents(testRelease("1.0.0", "fw.bin", "aaa-fw.bin.cab")))

		err := syncer.Promote(ctx, "beta", "preview")
		require.Error(t, err)
		assert.ErrorContains(t, err, "snapshot-beta.xml.zst does not exist")
	})

	t.Run("RejectsAnEmptyRepository", func(t *testing.T) {
		syncer, _ := createTestSyncer(t)
		assert.ErrorContains(t, syncer.Promote(ctx, "", "beta"), "snapshot.xml.zst does not exist")
	})

	t.Run("PromotesTheIndexIntoARing", func(t *testing.T) {
		syncer, tmpDir := createTestSyncer(t)
		index := testComponents(testRelease("1.0.0", "firmware-v1.bin", "aaa-firmware-v1.bin.cab"))
		writeTestMetadata(t, syncer, snapshotKey(""), index)

		require.NoError(t, syncer.Promote(ctx, "", "beta"))

		assert.Equal(t, []string{"1.0.0"}, releaseVersions(readTestMetadata(t, syncer, "snapshot-beta.xml.zst")))
		assert.Equal(t, []string{"1.0.0"}, releaseVersions(readTestMetadata(t, syncer, "metadata-beta.xml.zst")))
		assert.FileExists(t, filepath.Join(tmpDir, "output", "metadata-beta.xml.zst.jcat"))
		assert.NoFileExists(t, filepath.Join(tmpDir, "output", "snapshot-beta.xml.zst.jcat"),
			"a snapshot is internal, so it needs no signature")
	})

	t.Run("PromotesTheValidatedStateRatherThanTheLatestOne", func(t *testing.T) {
		syncer, _ := createTestSyncer(t)
		writeTestMetadata(t, syncer, snapshotKey(""), testComponents(testRelease("1.0.0", "firmware-v1.bin", "aaa-firmware-v1.bin.cab")))
		require.NoError(t, syncer.Promote(ctx, "", "beta"))

		// A later run mirrors newer firmware into the index.
		writeTestMetadata(t, syncer, snapshotKey(""), testComponents(
			testRelease("1.0.0", "firmware-v1.bin", "aaa-firmware-v1.bin.cab"),
			testRelease("2.0.0", "firmware-v2.bin", "bbb-firmware-v2.bin.cab"),
		))

		require.NoError(t, syncer.Promote(ctx, "beta", "preview"))

		assert.Equal(t, []string{"1.0.0"}, releaseVersions(readTestMetadata(t, syncer, "metadata-preview.xml.zst")),
			"preview must get what beta was serving, not whatever has been mirrored since")
		assert.Equal(t, []string{"1.0.0", "2.0.0"}, releaseVersions(readTestMetadata(t, syncer, snapshotKey(""))),
			"the index keeps holding the latest firmware")
	})

	t.Run("SnapshotsTheUnfilteredState", func(t *testing.T) {
		syncer, _ := createTestSyncer(t)
		syncer.Config.Blocklist = []string{"firmware-v2.bin"}
		writeTestMetadata(t, syncer, snapshotKey(""), testComponents(
			testRelease("1.0.0", "firmware-v1.bin", "aaa-firmware-v1.bin.cab"),
			testRelease("2.0.0", "firmware-v2.bin", "bbb-firmware-v2.bin.cab"),
		))

		require.NoError(t, syncer.Promote(ctx, "", "beta"))

		assert.Equal(t, []string{"1.0.0", "2.0.0"}, releaseVersions(readTestMetadata(t, syncer, "snapshot-beta.xml.zst")),
			"the snapshot keeps the blocked release so unblocking it needs no new promotion")
		assert.Equal(t, []string{"1.0.0"}, releaseVersions(readTestMetadata(t, syncer, "metadata-beta.xml.zst")))
	})

	t.Run("RecordsTheSnapshotBeforePublishingFromIt", func(t *testing.T) {
		syncer, _ := createTestSyncer(t)
		writeTestMetadata(t, syncer, snapshotKey(""), testComponents(testRelease("1.0.0", "firmware-v1.bin", "aaa-firmware-v1.bin.cab")))
		require.NoError(t, syncer.Promote(ctx, "", "beta"))

		writeTestMetadata(t, syncer, snapshotKey(""), testComponents(
			testRelease("1.0.0", "firmware-v1.bin", "aaa-firmware-v1.bin.cab"),
			testRelease("2.0.0", "firmware-v2.bin", "bbb-firmware-v2.bin.cab"),
		))
		local := syncer.Storage.(*LocalStorage)
		syncer.Storage = &failingWriteStorage{LocalStorage: local, failKey: feedKey("beta")}

		err := syncer.Promote(ctx, "", "beta")
		require.ErrorContains(t, err, "injected write failure")

		// Every feed is republished from the state its ring holds, so the
		// snapshot is what the ring converges to. Publishing before
		// recording it would have the next refresh roll the ring back to the
		// state it was promoted out of, without saying so.
		assert.Equal(t, []string{"1.0.0", "2.0.0"}, releaseVersions(readTestMetadata(t, syncer, snapshotKey("beta"))))
		assert.Equal(t, []string{"1.0.0"}, releaseVersions(readTestMetadata(t, syncer, feedKey("beta"))),
			"the feed is the one that failed to be written")

		syncer.Storage = local
		_, err = syncer.PublishFeeds(ctx)
		require.NoError(t, err)
		assert.Equal(t, []string{"1.0.0", "2.0.0"}, releaseVersions(readTestMetadata(t, syncer, feedKey("beta"))),
			"a later publish finishes the promotion rather than undoing it")
	})
}

func TestPublishFeeds(t *testing.T) {
	ctx := context.Background()

	t.Run("RepublishesEveryPromotedRing", func(t *testing.T) {
		syncer, _ := createTestSyncer(t)
		writeTestMetadata(t, syncer, snapshotKey(""), testComponents(
			testRelease("1.0.0", "firmware-v1.bin", "aaa-firmware-v1.bin.cab"),
			testRelease("2.0.0", "firmware-v2.bin", "bbb-firmware-v2.bin.cab"),
		))
		require.NoError(t, syncer.Promote(ctx, "", "beta"))
		require.NoError(t, syncer.Promote(ctx, "beta", "stable"))

		rings, err := syncer.listRings(ctx)
		require.NoError(t, err)
		assert.Equal(t, []string{"", "beta", "stable"}, rings,
			"the index is a ring, so a blocklist change reaches the hosts reading it too")

		// A release turns out to be bad and gets blocklisted.
		syncer.Config.Blocklist = []string{"firmware-v2.bin"}
		_, err = syncer.PublishFeeds(ctx)
		require.NoError(t, err)

		for _, ring := range rings {
			assert.Equal(t, []string{"1.0.0"}, releaseVersions(readTestMetadata(t, syncer, feedKey(ring))),
				"ring %s should have dropped the blocked release", ringLabel(ring))
			assert.Equal(t, []string{"1.0.0", "2.0.0"}, releaseVersions(readTestMetadata(t, syncer, snapshotKey(ring))),
				"ring %s should keep the blocked release in its state, so unblocking it needs no promotion", ringLabel(ring))
		}
	})

	t.Run("PublishesTheIndexFeedWithoutAnyPromotedRing", func(t *testing.T) {
		syncer, tmpDir := createTestSyncer(t)
		writeTestMetadata(t, syncer, snapshotKey(""), testComponents(testRelease("1.0.0", "fw.bin", "aaa-fw.bin.cab")))

		_, err := syncer.PublishFeeds(ctx)
		require.NoError(t, err)

		entries, err := os.ReadDir(filepath.Join(tmpDir, "output"))
		require.NoError(t, err)
		assert.Equal(t, []string{IndexKey, IndexKey + signatureSuffix, snapshotKey("")}, entryNames(entries))
	})

	t.Run("RepublishesARingLeftWithoutASnapshot", func(t *testing.T) {
		syncer, _ := createTestSyncer(t)
		// All a ring whose snapshot was removed leaves behind is the feed its
		// hosts keep downloading.
		writeTestMetadata(t, syncer, feedKey("stable"), testComponents(
			testRelease("1.0.0", "firmware-v1.bin", "aaa-firmware-v1.bin.cab"),
			testRelease("2.0.0", "firmware-v2.bin", "bbb-firmware-v2.bin.cab"),
		))
		syncer.Config.Blocklist = []string{"firmware-v2.bin"}

		_, err := syncer.PublishFeeds(ctx)
		require.NoError(t, err)

		assert.Equal(t, []string{"1.0.0"}, releaseVersions(readTestMetadata(t, syncer, feedKey("stable"))),
			"a blocklist entry has to reach a ring whose snapshot is gone as well")
		assert.Equal(t, []string{"1.0.0", "2.0.0"}, releaseVersions(readTestMetadata(t, syncer, snapshotKey("stable"))),
			"what it was serving becomes its state, so this is the last run that has to guess it")
	})

	t.Run("ReportsThePackagesEveryRingReferences", func(t *testing.T) {
		syncer, _ := createTestSyncer(t)
		writeTestMetadata(t, syncer, snapshotKey(""), testComponents(
			testRelease("1.0.0", "firmware-v1.bin", "aaa-firmware-v1.bin.cab")))
		require.NoError(t, syncer.Promote(ctx, "", "beta"))
		// The index moves on, and the release beta serves gets blocklisted.
		writeTestMetadata(t, syncer, snapshotKey(""), testComponents(
			testRelease("2.0.0", "firmware-v2.bin", "bbb-firmware-v2.bin.cab")))
		syncer.Config.Blocklist = []string{"firmware-v1.bin"}

		locations, err := syncer.PublishFeeds(ctx)

		require.NoError(t, err)
		assert.Equal(t, []string{"aaa-firmware-v1.bin.cab", "bbb-firmware-v2.bin.cab"}, slices.Sorted(maps.Keys(locations)),
			"a package a ring still holds in its state is not reclaimable, blocked or not")
	})
}

func TestSaveMetadataPublishesFeeds(t *testing.T) {
	ctx := context.Background()

	t.Run("RefreshesTheFeedsWhenNothingNewWasMirrored", func(t *testing.T) {
		syncer, _ := createTestSyncer(t)
		writeTestMetadata(t, syncer, snapshotKey(""), testComponents(
			testRelease("1.0.0", "firmware-v1.bin", "aaa-firmware-v1.bin.cab"),
			testRelease("2.0.0", "firmware-v2.bin", "bbb-firmware-v2.bin.cab"),
		))
		require.NoError(t, syncer.Promote(ctx, "", "stable"))
		require.NoError(t, syncer.LoadMetadata(ctx))

		syncer.Config.Blocklist = []string{"firmware-v2.bin"}
		require.Empty(t, syncer.newComponents)
		require.NoError(t, syncer.SaveMetadata(ctx))

		assert.Equal(t, []string{"1.0.0"}, releaseVersions(readTestMetadata(t, syncer, "metadata-stable.xml.zst")),
			"a blocklist entry must take effect on the next run even when no firmware was mirrored")
		assert.Equal(t, []string{"1.0.0"}, releaseVersions(readTestMetadata(t, syncer, IndexKey)),
			"including for the hosts reading the index feed")
	})

	t.Run("RefreshesTheFeedsAfterAnIndexUpdate", func(t *testing.T) {
		syncer, _ := createTestSyncer(t)
		writeTestMetadata(t, syncer, snapshotKey(""), testComponents(testRelease("1.0.0", "firmware-v1.bin", "aaa-firmware-v1.bin.cab")))
		require.NoError(t, syncer.Promote(ctx, "", "stable"))
		require.NoError(t, syncer.LoadMetadata(ctx))

		syncer.newComponents = testComponents(testRelease("2.0.0", "firmware-v2.bin", "bbb-firmware-v2.bin.cab")).Component
		syncer.Config.Blocklist = []string{"firmware-v1.bin"}
		require.NoError(t, syncer.SaveMetadata(ctx))

		assert.Equal(t, []string{"1.0.0", "2.0.0"}, releaseVersions(readTestMetadata(t, syncer, snapshotKey(""))),
			"the index holds everything mirrored so far")
		assert.Equal(t, []string{"2.0.0"}, releaseVersions(readTestMetadata(t, syncer, IndexKey)),
			"its feed is what the blocklist applies to")
		assert.Empty(t, readTestMetadata(t, syncer, "metadata-stable.xml.zst").Component,
			"stable only had the blocked release, so its feed is now empty")
	})
}

func TestPromoteRepublishesEveryRing(t *testing.T) {
	ctx := context.Background()
	syncer, _ := createTestSyncer(t)
	writeTestMetadata(t, syncer, snapshotKey(""), testComponents(
		testRelease("1.0.0", "firmware-v1.bin", "aaa-firmware-v1.bin.cab"),
		testRelease("2.0.0", "firmware-v2.bin", "bbb-firmware-v2.bin.cab"),
	))
	require.NoError(t, syncer.Promote(ctx, "", "beta"))
	require.NoError(t, syncer.Promote(ctx, "beta", "stable"))

	// A release turns out to be bad while only one ring is being promoted.
	syncer.Config.Blocklist = []string{"firmware-v2.bin"}
	require.NoError(t, syncer.Promote(ctx, "beta", "stable"))

	assert.Equal(t, []string{"1.0.0"}, releaseVersions(readTestMetadata(t, syncer, feedKey("beta"))),
		"a ring that was not the promotion target must still stop serving a blocked release")
	assert.Equal(t, []string{"1.0.0"}, releaseVersions(readTestMetadata(t, syncer, feedKey("stable"))))
}
