package main

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/alecthomas/kong"
	"github.com/premday/firmirror/pkg/bkc"
	"github.com/premday/firmirror/pkg/firmirror"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// withArgs runs a test with its own command line, and puts the real one back.
func withArgs(t *testing.T) {
	t.Helper()
	saved := args
	t.Cleanup(func() { args = saved })
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

func TestANewFileHasTheRingsOfTheLibraryUnlessToldOtherwise(t *testing.T) {
	withArgs(t)
	path := filepath.Join(t.TempDir(), "bkc.yml")

	file, err := promotionBase(path)
	require.NoError(t, err)
	assert.Equal(t, bkc.DefaultRings, file.Rings)

	args.BKC.Rings = []string{"canary", "all"}
	file, err = promotionBase(path)
	require.NoError(t, err)
	assert.Equal(t, []string{"canary", "all"}, file.Rings)
}

func TestTheRingsAreSaidInTheHelp(t *testing.T) {
	withArgs(t)

	var help strings.Builder
	parser, err := kong.New(&args, parserVars, kong.Writers(&help, &help), kong.Exit(func(int) {}))
	require.NoError(t, err)

	_, _ = parser.Parse([]string{"--help"})

	assert.Contains(t, strings.Join(strings.Fields(help.String()), " "), "Default: dev,beta,preview,stable.")
}

func TestOpenSyncerDoesNotCreateWhatItWasPointedAtByMistake(t *testing.T) {
	withArgs(t)
	args.OutputDir = filepath.Join(t.TempDir(), "typo")

	_, _, err := openSyncer(context.Background(), firmirror.FirmirrorConfig{})

	assert.ErrorContains(t, err, "output directory")
	assert.NoDirExists(t, args.OutputDir)
}
