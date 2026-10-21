package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRegenMetadataNeedsWhatItSigns(t *testing.T) {
	withArgs(t)
	t.Setenv("PATH", t.TempDir())

	assert.ErrorContains(t, runRegenMetadata(context.Background()), "jcat-tool is required")
}

func TestRegenMetadataDoesNotPublishWhenToldToEndWhileLoading(t *testing.T) {
	withArgs(t)
	bin := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(bin, "jcat-tool"), []byte("#!/bin/sh\n"), 0o755))
	t.Setenv("PATH", bin)
	args.OutputDir = t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	assert.ErrorIs(t, runRegenMetadata(ctx), context.Canceled)
}
