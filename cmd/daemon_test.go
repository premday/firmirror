package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/premday/firmirror/pkg/bkc"
	"github.com/premday/firmirror/pkg/daemon"
	"github.com/premday/firmirror/pkg/firmirror"
)

func TestDaemonErrorKeepsTheDaemonUpOnlyWhenThereIsAnythingToServe(t *testing.T) {
	cleanup := fmt.Errorf("metadata saved but %w: denied", firmirror.ErrCleanup)
	other := errors.New("could not write")

	tests := map[string]struct {
		outcome refreshOutcome
		err     error
		told    bool
		partial bool
		failed  bool
	}{
		"fine":                         {outcome: refreshOutcome{holdsMetadata: true}},
		"mirrored, not published":      {outcome: refreshOutcome{holdsMetadata: true, newComponents: 2}, partial: true, failed: true},
		"mirrored and published":       {outcome: refreshOutcome{holdsMetadata: true, newComponents: 2, published: true}},
		"vendor failed, document held": {outcome: refreshOutcome{vendorsFailed: true, holdsMetadata: true}, partial: true, failed: true},
		"vendors failed, nothing held": {outcome: refreshOutcome{vendorsFailed: true}, failed: true},
		"rings in use published":       {outcome: refreshOutcome{holdsMetadata: true, fellBack: true}, partial: true, failed: true},
		"nothing mirrored or stored":   {failed: true},
		"told to end, nothing yet":     {told: true},
		"cleanup failed":               {outcome: refreshOutcome{holdsMetadata: true}, err: errors.Join(cleanup), partial: true, failed: true},
		"anything else":                {err: other, failed: true},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			err := daemonError(test.outcome, test.err, test.told)

			assert.Equal(t, test.failed, err != nil)
			assert.Equal(t, test.partial, errors.Is(err, daemon.ErrPartial))
		})
	}
}

func TestDaemonErrorKeepsTheVendorFailuresBesideAPlainOne(t *testing.T) {
	err := daemonError(refreshOutcome{holdsMetadata: true, vendorsFailed: true}, errors.New("could not write"), false)

	assert.NotErrorIs(t, err, daemon.ErrPartial)
	assert.ErrorIs(t, err, errVendorsFailed)
	assert.ErrorContains(t, err, "could not write")
}

func TestDaemonErrorKeepsTheVendorFailuresBesideTheCleanupOnes(t *testing.T) {
	cleanup := fmt.Errorf("metadata saved but %w: denied", firmirror.ErrCleanup)

	err := daemonError(refreshOutcome{holdsMetadata: true, vendorsFailed: true}, cleanup, false)

	assert.ErrorIs(t, err, daemon.ErrPartial)
	assert.ErrorIs(t, err, errVendorsFailed)
	assert.ErrorIs(t, err, firmirror.ErrCleanup)
}

func TestRingsInForceKeepTheOnesInUseWhenTheFileCannotBeRead(t *testing.T) {
	withArgs(t)
	ctx := context.Background()
	inUse := bkc.New([]string{"dev"})
	file := filepath.Join(t.TempDir(), "bkc.yml")
	args.BKC.File = file

	assert.Same(t, inUse, ringsInForce(ctx, inUse), "a missing file is no reason to stop mirroring")

	require.NoError(t, os.WriteFile(file, []byte("rings: ["), 0o644))
	assert.Same(t, inUse, ringsInForce(ctx, inUse), "nor is an invalid one")

	fresh := bkc.New([]string{"dev", "beta"})
	require.NoError(t, fresh.Write(file))
	assert.Equal(t, []string{"dev", "beta"}, ringsInForce(ctx, inUse).Rings)

	args.BKC.File = ""
	assert.Nil(t, ringsInForce(ctx, inUse), "without a file there are no rings")
}
