package firmirror

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/premday/firmirror/pkg/lvfs"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPruneSupersededReleases(t *testing.T) {
	t.Run("KeepsTheRebuildAndDropsWhatItReplaced", func(t *testing.T) {
		// The shape refresh leaves behind: one component, the same vendor
		// firmware packaged twice, the rebuild appended last.
		stored := &lvfs.Components{Origin: "firmirror", Component: []lvfs.Component{{
			ID: "com.test.firmware",
			Releases: []lvfs.Release{
				testRelease("1.0.0", "firmware.bin", "old-rebuilt.cab"),
				testRelease("1.0.0", "firmware.bin", "new-rebuilt.cab"),
			},
		}}}

		pruned, superseded := pruneSupersededReleases(stored)

		require.Len(t, pruned.Component, 1)
		require.Len(t, pruned.Component[0].Releases, 1)
		assert.Equal(t, "new-rebuilt.cab", pruned.Component[0].Releases[0].Location)
		assert.Equal(t, []string{"old-rebuilt.cab"}, superseded)
	})

	t.Run("KeepsReleasesOfDifferentFirmware", func(t *testing.T) {
		stored := &lvfs.Components{Origin: "firmirror", Component: []lvfs.Component{{
			ID: "com.test.firmware",
			Releases: []lvfs.Release{
				testRelease("1.0.0", "firmware-v1.bin", "aaa.cab"),
				testRelease("2.0.0", "firmware-v2.bin", "bbb.cab"),
			},
		}}}

		pruned, superseded := pruneSupersededReleases(stored)

		assert.Len(t, pruned.Component[0].Releases, 2, "two versions of a firmware are not a rebuild of each other")
		assert.Empty(t, superseded)
	})

	t.Run("KeepsTheSameFirmwareUnderDifferentComponents", func(t *testing.T) {
		release := testRelease("1.0.0", "firmware.bin", "shared.cab")
		stored := &lvfs.Components{Origin: "firmirror", Component: []lvfs.Component{
			{ID: "com.test.one", Releases: []lvfs.Release{release}},
			{ID: "com.test.two", Releases: []lvfs.Release{release}},
		}}

		pruned, superseded := pruneSupersededReleases(stored)

		assert.Len(t, pruned.Component[0].Releases, 1)
		assert.Len(t, pruned.Component[1].Releases, 1)
		assert.Empty(t, superseded, "components are pruned independently: each one applies to its own devices")
	})

	t.Run("LeavesAReleaseWithoutAFirmwareFilenameAlone", func(t *testing.T) {
		stored := &lvfs.Components{Origin: "firmirror", Component: []lvfs.Component{{
			ID: "com.test.firmware",
			Releases: []lvfs.Release{
				{Version: "1.0.0", Location: "aaa.cab"},
				{Version: "2.0.0", Location: "bbb.cab"},
			},
		}}}

		pruned, superseded := pruneSupersededReleases(stored)

		assert.Len(t, pruned.Component[0].Releases, 2)
		assert.Empty(t, superseded)
	})

	t.Run("HandlesNoMetadata", func(t *testing.T) {
		pruned, superseded := pruneSupersededReleases(nil)

		assert.Nil(t, pruned)
		assert.Empty(t, superseded)
	})
}

func TestCleanupPackages(t *testing.T) {
	ctx := context.Background()

	newCleanupSyncer := func(t *testing.T, deleteFailures map[string]int) (*FirmirrorSyncer, string) {
		t.Helper()
		tmpDir := t.TempDir()
		local, err := NewLocalStorage(tmpDir)
		require.NoError(t, err)
		if deleteFailures == nil {
			deleteFailures = make(map[string]int)
		}
		storage := &cleanupStorage{LocalStorage: local, deleteFailures: deleteFailures}
		syncer, err := NewFirmirrorSyncer(FirmirrorConfig{CacheDir: filepath.Join(tmpDir, "cache")}, storage)
		require.NoError(t, err)
		return syncer, tmpDir
	}

	t.Run("ReplacesASupersededReleaseAndReclaimsItsPackage", func(t *testing.T) {
		syncer, tmpDir := newCleanupSyncer(t, nil)
		for _, cab := range []string{"old-rebuilt.cab", "new-rebuilt.cab"} {
			require.NoError(t, os.WriteFile(filepath.Join(tmpDir, cab), []byte("cab"), 0644))
		}
		// What a refresh leaves behind once a vendor rebuilt a package.
		writeTestMetadata(t, syncer, snapshotKey(""), &lvfs.Components{
			Origin:        "firmirror",
			SchemaVersion: lvfs.MetadataSchemaVersion,
			Component: []lvfs.Component{{
				ID: "com.test.firmware",
				Releases: []lvfs.Release{
					testRelease("1.0.0", "firmware.bin", "old-rebuilt.cab"),
					testRelease("1.0.0", "firmware.bin", "new-rebuilt.cab"),
				},
			}},
		})

		require.NoError(t, syncer.CleanupPackages(ctx))

		index := readTestMetadata(t, syncer, snapshotKey(""))
		require.Len(t, index.Component, 1)
		require.Len(t, index.Component[0].Releases, 1, "the superseded release is what --s3.cleanup used to replace")
		assert.Equal(t, "new-rebuilt.cab", index.Component[0].Releases[0].Location)
		assert.NoFileExists(t, filepath.Join(tmpDir, "old-rebuilt.cab"))
		assert.FileExists(t, filepath.Join(tmpDir, "new-rebuilt.cab"))
		assert.FileExists(t, filepath.Join(tmpDir, IndexKey+".jcat"), "the index feed is republished and signed")
	})

	t.Run("ReplacesASupersededReleaseButKeepsAPackageARingServes", func(t *testing.T) {
		syncer, tmpDir := newCleanupSyncer(t, nil)
		for _, cab := range []string{"old-rebuilt.cab", "new-rebuilt.cab"} {
			require.NoError(t, os.WriteFile(filepath.Join(tmpDir, cab), []byte("cab"), 0644))
		}
		superseded := &lvfs.Components{
			Origin:        "firmirror",
			SchemaVersion: lvfs.MetadataSchemaVersion,
			Component: []lvfs.Component{{
				ID:       "com.test.firmware",
				Releases: []lvfs.Release{testRelease("1.0.0", "firmware.bin", "old-rebuilt.cab")},
			}},
		}
		// stable was promoted while the old package was the current one.
		writeTestMetadata(t, syncer, snapshotKey("stable"), superseded)
		writeTestMetadata(t, syncer, feedKey("stable"), superseded)
		writeTestMetadata(t, syncer, snapshotKey(""), &lvfs.Components{
			Origin:        "firmirror",
			SchemaVersion: lvfs.MetadataSchemaVersion,
			Component: []lvfs.Component{{
				ID: "com.test.firmware",
				Releases: []lvfs.Release{
					testRelease("1.0.0", "firmware.bin", "old-rebuilt.cab"),
					testRelease("1.0.0", "firmware.bin", "new-rebuilt.cab"),
				},
			}},
		})

		require.NoError(t, syncer.CleanupPackages(ctx))

		assert.Equal(t, []string{"1.0.0"}, releaseVersions(readTestMetadata(t, syncer, snapshotKey(""))))
		assert.FileExists(t, filepath.Join(tmpDir, "old-rebuilt.cab"),
			"stable still serves it, so pruning the index must not reclaim it")
	})

	t.Run("LeavesTheIndexAloneWhenNothingIsSuperseded", func(t *testing.T) {
		syncer, tmpDir := newCleanupSyncer(t, nil)
		require.NoError(t, os.WriteFile(filepath.Join(tmpDir, "aaa-firmware-v1.bin.cab"), []byte("cab"), 0644))
		writeTestMetadata(t, syncer, snapshotKey(""), testComponents(testRelease("1.0.0", "firmware-v1.bin", "aaa-firmware-v1.bin.cab")))
		before, err := os.ReadFile(filepath.Join(tmpDir, snapshotKey("")))
		require.NoError(t, err)

		require.NoError(t, syncer.CleanupPackages(ctx))

		after, err := os.ReadFile(filepath.Join(tmpDir, snapshotKey("")))
		require.NoError(t, err)
		assert.Equal(t, before, after, "an index with nothing to replace is not rewritten")
	})

	t.Run("DropsASupersededReleaseThatFreesNoPackage", func(t *testing.T) {
		syncer, _ := newCleanupSyncer(t, nil)
		// The older release of the pair carries no location, so replacing it
		// reclaims nothing; leaving it there would have the index carry two
		// releases of one firmware forever.
		writeTestMetadata(t, syncer, snapshotKey(""), &lvfs.Components{
			Origin:        "firmirror",
			SchemaVersion: lvfs.MetadataSchemaVersion,
			Component: []lvfs.Component{{
				ID: "com.test.firmware",
				Releases: []lvfs.Release{
					{Version: "1.0.0", Checksums: []lvfs.Checksum{{Filename: "firmware.bin"}}},
					testRelease("1.0.0", "firmware.bin", "new-rebuilt.cab"),
				},
			}},
		})

		require.NoError(t, syncer.CleanupPackages(ctx))

		index := readTestMetadata(t, syncer, snapshotKey(""))
		require.Len(t, index.Component, 1)
		require.Len(t, index.Component[0].Releases, 1)
		assert.Equal(t, "new-rebuilt.cab", index.Component[0].Releases[0].Location)
	})

	t.Run("ReportsNothingReclaimableWhenTheIndexWasNeverLoaded", func(t *testing.T) {
		syncer, tmpDir := newCleanupSyncer(t, nil)
		require.NoError(t, os.WriteFile(filepath.Join(tmpDir, "aaa-firmware-v1.bin.cab"), []byte("cab"), 0644))
		writeTestMetadata(t, syncer, snapshotKey(""), testComponents(testRelease("1.0.0", "firmware-v1.bin", "aaa-firmware-v1.bin.cab")))
		// What a run whose metadata failed to load holds: nothing. The
		// packages to keep come from the documents in storage, never from
		// what the run happens to have in memory.
		require.Nil(t, syncer.existingMetadata)
		logs := captureLogs(t)

		require.NoError(t, syncer.SaveMetadata(ctx))

		assert.NotContains(t, logs.String(), "run s3-cleanup to remove them")
		assert.FileExists(t, filepath.Join(tmpDir, "aaa-firmware-v1.bin.cab"))
	})

	t.Run("KeepsPackagesAnOlderRingStillPointsAt", func(t *testing.T) {
		syncer, tmpDir := newCleanupSyncer(t, nil)
		for _, cab := range []string{"aaa-firmware-v1.bin.cab", "bbb-firmware-v2.bin.cab", "ccc-orphan.bin.cab"} {
			require.NoError(t, os.WriteFile(filepath.Join(tmpDir, cab), []byte("cab"), 0644))
		}

		// stable still serves v1 while the index has moved on to v2.
		writeTestMetadata(t, syncer, snapshotKey(""), testComponents(testRelease("1.0.0", "firmware-v1.bin", "aaa-firmware-v1.bin.cab")))
		require.NoError(t, syncer.Promote(ctx, "", "stable"))
		index := testComponents(testRelease("2.0.0", "firmware-v2.bin", "bbb-firmware-v2.bin.cab"))
		writeTestMetadata(t, syncer, snapshotKey(""), index)

		require.NoError(t, syncer.CleanupPackages(ctx))

		assert.FileExists(t, filepath.Join(tmpDir, "aaa-firmware-v1.bin.cab"), "still referenced by the stable ring")
		assert.FileExists(t, filepath.Join(tmpDir, "bbb-firmware-v2.bin.cab"), "referenced by the index")
		assert.NoFileExists(t, filepath.Join(tmpDir, "ccc-orphan.bin.cab"), "referenced by nothing")
	})

	t.Run("KeepsThePackagesOfARingLeftWithoutASnapshot", func(t *testing.T) {
		syncer, tmpDir := newCleanupSyncer(t, nil)
		require.NoError(t, os.WriteFile(filepath.Join(tmpDir, "aaa-firmware-v1.bin.cab"), []byte("cab"), 0644))

		// All a ring whose snapshot was removed by hand leaves behind is the
		// feed its hosts keep downloading.
		writeTestMetadata(t, syncer, feedKey("stable"), testComponents(testRelease("1.0.0", "firmware-v1.bin", "aaa-firmware-v1.bin.cab")))

		require.NoError(t, syncer.CleanupPackages(ctx))

		assert.FileExists(t, filepath.Join(tmpDir, "aaa-firmware-v1.bin.cab"),
			"stable still serves it, so the hosts on it must not get a 404")
		assert.Equal(t, []string{"1.0.0"}, releaseVersions(readTestMetadata(t, syncer, snapshotKey("stable"))),
			"the state it is serving is recorded, so this is the last run that has to guess it")
	})

	t.Run("ReportsAReferencedPackageThatIsMissing", func(t *testing.T) {
		syncer, _ := newCleanupSyncer(t, nil)
		writeTestMetadata(t, syncer, snapshotKey(""), testComponents(testRelease("1.0.0", "firmware-v1.bin", "aaa-firmware-v1.bin.cab")))
		logs := captureLogs(t)

		require.NoError(t, syncer.CleanupPackages(ctx))

		assert.Contains(t, logs.String(), "aaa-firmware-v1.bin.cab")
		assert.Contains(t, logs.String(), "missing from storage")
	})

	t.Run("DeletesAnUnreferencedPackage", func(t *testing.T) {
		syncer, tmpDir := newCleanupSyncer(t, nil)
		require.NoError(t, os.WriteFile(filepath.Join(tmpDir, "ccc-orphan.bin.cab"), []byte("cab"), 0644))

		require.NoError(t, syncer.CleanupPackages(ctx))

		assert.NoFileExists(t, filepath.Join(tmpDir, "ccc-orphan.bin.cab"))
	})

	t.Run("KeepsAPackageTooRecentToTellApartFromARunInProgress", func(t *testing.T) {
		// What a refresh in progress looks like: cabinets uploaded as it
		// builds them, with the metadata naming them not published yet.
		syncer, tmpDir := newCleanupSyncer(t, nil)
		syncer.Config.MinPackageAge = 24 * time.Hour
		inFlight := filepath.Join(tmpDir, "in-flight.cab")
		stale := filepath.Join(tmpDir, "stale.cab")
		require.NoError(t, os.WriteFile(inFlight, []byte("cab"), 0644))
		require.NoError(t, os.WriteFile(stale, []byte("cab"), 0644))
		old := time.Now().Add(-48 * time.Hour)
		require.NoError(t, os.Chtimes(stale, old, old))
		logs := captureLogs(t)

		require.NoError(t, syncer.CleanupPackages(ctx))

		assert.FileExists(t, inFlight, "deleting it would take away what a run in progress is about to publish")
		assert.NoFileExists(t, stale, "nothing has referenced it for long enough to be sure it is an orphan")
		assert.Contains(t, logs.String(), "too recent")
	})

	t.Run("ReportsADeletionThatFailedSoALaterRunRetriesIt", func(t *testing.T) {
		syncer, tmpDir := newCleanupSyncer(t, map[string]int{"ccc-orphan.bin.cab": 1})
		require.NoError(t, os.WriteFile(filepath.Join(tmpDir, "ccc-orphan.bin.cab"), []byte("cab"), 0644))

		require.ErrorContains(t, syncer.CleanupPackages(ctx), "transient delete failure")
		assert.FileExists(t, filepath.Join(tmpDir, "ccc-orphan.bin.cab"))

		require.NoError(t, syncer.CleanupPackages(ctx))
		assert.NoFileExists(t, filepath.Join(tmpDir, "ccc-orphan.bin.cab"))
	})

	t.Run("RefusesAStorageBackendThatCannotDelete", func(t *testing.T) {
		syncer, _ := createTestSyncer(t)

		err := syncer.CleanupPackages(ctx)

		assert.ErrorContains(t, err, "does not support deleting packages",
			"a local repository keeps its old packages, so cleanup must say so rather than silently do nothing")
	})

	t.Run("NeverDeletesMetadataDocuments", func(t *testing.T) {
		syncer, tmpDir := newCleanupSyncer(t, nil)
		writeTestMetadata(t, syncer, snapshotKey(""), testComponents(testRelease("1.0.0", "firmware-v1.bin", "aaa-firmware-v1.bin.cab")))
		require.NoError(t, syncer.Promote(ctx, "", "stable"))

		require.NoError(t, syncer.CleanupPackages(ctx))

		for _, key := range []string{IndexKey, snapshotKey(""), "snapshot-stable.xml.zst", "metadata-stable.xml.zst", "metadata-stable.xml.zst.jcat"} {
			assert.FileExists(t, filepath.Join(tmpDir, key))
		}
	})
}

func TestLocalStorageList(t *testing.T) {
	t.Run("ListsKeysUnderThePrefix", func(t *testing.T) {
		tmpDir := t.TempDir()
		storage, err := NewLocalStorage(tmpDir)
		require.NoError(t, err)
		for _, key := range []string{"metadata.xml.zst", "snapshot-beta.xml.zst", "snapshot-stable.xml.zst", "aaa.cab"} {
			require.NoError(t, storage.Write(context.Background(), key, bytes.NewReader([]byte("x"))))
		}

		objects, err := storage.List(context.Background(), "snapshot-")
		require.NoError(t, err)
		assert.ElementsMatch(t, []string{"snapshot-beta.xml.zst", "snapshot-stable.xml.zst"}, objectKeys(objects))

		all, err := storage.List(context.Background(), "")
		require.NoError(t, err)
		assert.Len(t, all, 4)
		for _, object := range all {
			assert.False(t, object.ModifiedAt.IsZero(), "%s should carry a modification time", object.Key)
		}
	})

	t.Run("ListsNestedKeysWithSlashes", func(t *testing.T) {
		tmpDir := t.TempDir()
		storage, err := NewLocalStorage(tmpDir)
		require.NoError(t, err)
		require.NoError(t, storage.Write(context.Background(), "nested/dir/file.cab", bytes.NewReader([]byte("x"))))

		objects, err := storage.List(context.Background(), "")
		require.NoError(t, err)
		assert.Equal(t, []string{"nested/dir/file.cab"}, objectKeys(objects))
	})
}

func TestLocalStorageLock(t *testing.T) {
	ctx := context.Background()
	tmpDir := t.TempDir()
	first, err := NewLocalStorage(tmpDir)
	require.NoError(t, err)
	second, err := NewLocalStorage(tmpDir)
	require.NoError(t, err)

	lock, err := first.Lock(ctx)
	require.NoError(t, err)
	_, err = second.Lock(ctx)
	assert.ErrorIs(t, err, ErrRepositoryLocked)

	require.NoError(t, lock.Release(ctx))
	lock, err = second.Lock(ctx)
	require.NoError(t, err)
	require.NoError(t, lock.Release(ctx))
}

func TestRepositoryLockContexts(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	storage, err := NewLocalStorage(t.TempDir())
	require.NoError(t, err)
	lock, err := storage.Lock(ctx)
	require.NoError(t, err)

	// A run ended by a signal still owes its repository the metadata it
	// mirrored, so that commit runs on a context the signal does not cancel.
	cancel()
	assert.Error(t, lock.Work.Err(), "the work a signal interrupts has to stop")
	assert.NoError(t, lock.Held.Err(), "the commit it still owes must not stop with it")

	require.NoError(t, lock.Release(context.Background()))
	assert.Error(t, lock.Held.Err(), "nothing may be written once the lock is gone")
}
