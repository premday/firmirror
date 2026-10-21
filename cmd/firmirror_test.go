package main

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/premday/firmirror/pkg/bkc"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// withArgs runs a test with its own command line, and puts the real one back.
func withArgs(t *testing.T) {
	t.Helper()
	saved := args
	t.Cleanup(func() { args = saved })
	args.BKC.Rings = bkc.DefaultRings
}

func TestLoadDefinitions(t *testing.T) {
	withArgs(t)
	ctx := context.Background()

	definitions, err := loadDefinitions(ctx)
	require.NoError(t, err)
	assert.Nil(t, definitions, "no file, no ring")

	args.BKC.File = filepath.Join(t.TempDir(), "missing.yml")
	_, err = loadDefinitions(ctx)
	assert.Error(t, err, "a refresh must not publish without rings when it was told where they are")
}
