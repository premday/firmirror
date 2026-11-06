package firmirror

import (
	"context"
	"os"
	"testing"

	"github.com/premday/firmirror/pkg/bkc"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestARefusedPublishMatchesErrRefusedAndKeepsItsMessage(t *testing.T) {
	ctx := context.Background()
	empty := newRingsSyncer(t, t.TempDir(), definitions("10.0"))
	require.NoError(t, empty.LoadMetadata(ctx))
	err := empty.RegenerateMetadata(ctx)
	assert.ErrorIs(t, err, ErrRefused)
	assert.EqualError(t, err, "there is no metadata: run a refresh first")

	dir := t.TempDir()
	publishSigned(t, dir)
	assert.ErrorIs(t, unsignedRun(t, dir).Preflight(ctx), ErrRefused, "the signature would be replaced")

	tagged := t.TempDir()
	refresh(t, tagged, definitions("10.0"), versioned("10.0", "a.cab"))
	forgetful := newRingsSyncer(t, tagged, definitions())
	require.NoError(t, forgetful.LoadMetadata(ctx))
	err = forgetful.Preflight(ctx)
	assert.ErrorIs(t, err, ErrRefused, "the ring tags would be dropped")
	assert.ErrorIs(t, err, ErrRingsRefused, "which other rings would lift")
	assert.NotErrorIs(t, unsignedRun(t, dir).Preflight(ctx), ErrRingsRefused, "no choice of rings lifts the lack of signing keys")
}

func TestAJCATThatCouldNotBeReadIsNotARefusal(t *testing.T) {
	quickJCATRetries(t)
	dir := t.TempDir()
	publishSigned(t, dir)
	require.NoError(t, os.WriteFile(jcatPath(dir), []byte{0x1f, 0x8b, 0x08}, 0o644))

	err := unsignedRun(t, dir).Preflight(context.Background())

	require.Error(t, err)
	assert.NotErrorIs(t, err, ErrRefused, "what could not be read may be readable next time")
}

func TestPreflightFallsBackToTheRingsInUseThatWouldBeRefused(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	inUse := definitions("10.0", "10.0")
	refresh(t, dir, inUse, versioned("10.0", "a.cab"))
	fewer := bkc.New([]string{"dev"})

	syncer := newRingsSyncer(t, dir, fewer)
	require.NoError(t, syncer.LoadMetadata(ctx))
	assert.ErrorIs(t, syncer.Preflight(ctx), ErrRingsRefused, "nothing to fall back to")
	assert.False(t, syncer.FellBack())

	syncer.Config.InUseBKC = inUse
	require.NoError(t, syncer.Preflight(ctx))
	assert.True(t, syncer.FellBack())
	assert.Same(t, inUse, syncer.Config.BKC)
}

func TestAFixedRingsFileReadAgainIsNotReportedAsAFallBack(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	inUse := definitions("10.0", "10.0")
	refresh(t, dir, inUse, versioned("10.0", "a.cab"))

	syncer := newRingsSyncer(t, dir, bkc.New([]string{"dev"}))
	syncer.Config.InUseBKC = inUse
	fixed := definitions("10.0", "10.0")
	syncer.Config.ReloadBKC = func(context.Context) (*bkc.File, error) { return fixed, nil }
	require.NoError(t, syncer.LoadMetadata(ctx))
	require.NoError(t, syncer.Preflight(ctx))
	require.True(t, syncer.FellBack(), "the file was bad when the run began")

	require.NoError(t, syncer.SaveMetadata(ctx))

	assert.Same(t, fixed, syncer.Config.BKC)
	assert.False(t, syncer.FellBack(), "and was fixed meanwhile, which is what is published")
}
