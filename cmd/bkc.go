package main

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"text/tabwriter"

	"github.com/premday/firmirror/pkg/bkc"
	"github.com/premday/firmirror/pkg/firmirror"
	"github.com/premday/firmirror/pkg/lvfs"
	"github.com/premday/firmirror/pkg/utils"
)

func runBKCStatus(ctx context.Context) error {
	if args.BKC.File == "" {
		return errors.New("--bkc.file is required to report the rollout rings")
	}
	file, err := loadRings(ctx, args.BKC.File)
	if err != nil {
		return err
	}
	shown := file.Rings
	if ring := args.Rollout.Status.Ring; ring != "" {
		if err := file.CheckRing(ring); err != nil {
			return err
		}
		shown = []string{ring}
	}

	table := newTable()
	fmt.Fprintf(table, "COMPONENT\tPLATFORM\tNAME\t%s\n", strings.ToUpper(strings.Join(shown, "\t")))
	for _, entry := range file.Components {
		versions := make([]string, 0, len(shown))
		for _, ring := range shown {
			versions = append(versions, cmp.Or(entry.Versions[ring], "-"))
		}
		fmt.Fprintf(table, "%s\t%s\t%s\t%s\n", entry.ID, cmp.Or(entry.Platform, "-"), bkc.OneLine(entry.Name), strings.Join(versions, "\t"))
	}
	return table.Flush()
}

// mirroredComponents are the components a promotion chooses from: those of the
// published metadata given, else of the storage.
func mirroredComponents(ctx context.Context) ([]lvfs.Component, error) {
	if source := args.Rollout.Promote.Metadata; source != "" {
		// It ends up in the file that decides what every host is served, so it is
		// read as strictly as that file is.
		components, err := firmirror.ReadMetadata(ctx, source, !args.BKC.AllowHTTP)
		return components, allowHTTPHint(err)
	}
	if err := checkOutputDir(); err != nil {
		return nil, err
	}
	storage, err := openStorage(ctx)
	if err != nil {
		return nil, err
	}
	return firmirror.ReadStoredComponents(ctx, storage)
}

// readLocal reads a file, up to the size a file may be. It is read as a path
// whatever it looks like, as it is written back to the same path.
func readLocal(path string) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	return utils.ReadAllLimited(file, bkc.MaxFileSize, path)
}

// promotionBase is the file a promotion starts from, or a new one when there is
// none yet.
func promotionBase(path string) (*bkc.File, error) {
	// A missing file, or one with only whitespace and comments, is created.
	data, readErr := readLocal(path)
	if readErr != nil && !errors.Is(readErr, os.ErrNotExist) {
		return nil, fmt.Errorf("reading the rollout rings: %w", readErr)
	}
	if readErr != nil || bkc.Blank(data) {
		rings := args.BKC.Rings
		if len(rings) == 0 {
			rings = bkc.DefaultRings
		}
		file := bkc.New(rings)
		if err := file.Validate(); err != nil {
			return nil, fmt.Errorf("invalid --bkc.rings: %w", err)
		}
		return file, nil
	}
	file, err := bkc.Parse(data)
	if err != nil {
		return nil, fmt.Errorf("reading the rollout rings from %s: %w", path, err)
	}
	if len(args.BKC.Rings) > 0 && !slices.Equal(args.BKC.Rings, file.Rings) {
		slog.Warn("--bkc.rings only names the rings of a new file, and this one keeps its own", "rings", file.Rings)
	}
	return file, nil
}

func runBKCPromote(ctx context.Context) error {
	command := args.Rollout.Promote
	if args.BKC.File == "" {
		return errors.New("--bkc.file is required to promote a rollout ring")
	}
	path, local := utils.LocalPath(args.BKC.File)
	if !local {
		return fmt.Errorf("--bkc.file is %s: a promotion edits a file, so it has to be a path", utils.Redacted(args.BKC.File))
	}
	// Fail on a missing directory before the table is shown.
	target, err := bkc.FollowLinks(path)
	if err != nil {
		return err
	}
	if _, err := os.Stat(filepath.Dir(target)); err != nil {
		return fmt.Errorf("directory of --bkc.file: %w", err)
	}
	if command.Prune && len(command.Component) > 0 {
		return errors.New("--prune cannot be combined with --component: it removes entries of components the mirror does not have, which cannot be named")
	}
	base, err := promotionBase(path)
	if err != nil {
		return err
	}
	if err := base.CheckRing(command.Ring); err != nil {
		return err
	}

	components, err := mirroredComponents(ctx)
	if err != nil {
		return err
	}
	promoted, changes, err := base.Promote(components, command.Ring, command.Component)
	if err != nil {
		return err
	}
	var removed []bkc.Entry
	if command.Prune {
		promoted, removed = promoted.Prune(components)
	}
	// An entry can be completed without any version moving, which is written too.
	if promoted.Equal(base) {
		slog.Info("Nothing to promote", "ring", command.Ring)
		return nil
	}

	table := newTable()
	fmt.Fprintf(table, "COMPONENT\tPLATFORM\tNAME\tFROM\tTO\t\n")
	for _, change := range changes {
		fmt.Fprintf(table, "%s\t%s\t%s\t%s\t%s\t%s\n", change.ComponentID, cmp.Or(change.Platform, "-"), bkc.OneLine(change.Name), cmp.Or(change.From, "-"), change.To, change.Note())
	}
	for _, entry := range removed {
		fmt.Fprintf(table, "%s\t%s\t%s\t\t\tremoved, the mirror does not have it\n", entry.ID, cmp.Or(entry.Platform, "-"), bkc.OneLine(entry.Name))
	}
	if err := table.Flush(); err != nil {
		return err
	}

	if len(removed)*2 > len(base.Components) && !command.AllowMassPrune {
		if !command.DryRun {
			return fmt.Errorf("--prune would remove %d of the %d entries of the file: the mirror may be partial or of another environment; pass --allow-mass-prune if it is not",
				len(removed), len(base.Components))
		}
		slog.Warn("This would remove most of the file, and the real run refuses unless --allow-mass-prune is passed",
			"removed", len(removed), "entries", len(base.Components))
	}
	if downgrades := bkc.CountDowngrades(changes); downgrades > 0 && !command.AllowDowngrade {
		if !command.DryRun {
			return fmt.Errorf("promoting %q would downgrade %d component(s), and fwupdmgr sync would downgrade the hosts on the ring: pass --allow-downgrade to do it", command.Ring, downgrades)
		}
		// A preview warns instead of failing.
		slog.Warn("This promotion would downgrade hosts, and the real run refuses unless --allow-downgrade is passed",
			"ring", command.Ring, "components", downgrades)
	}
	if command.DryRun {
		slog.Info("Dry run, nothing written", "ring", command.Ring, "changes", len(changes), "removed", len(removed))
		return nil
	}

	if err := promoted.Write(path); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	slog.Info("Ring promoted in the file, commit it to publish it", "ring", command.Ring, "changes", len(changes), "removed", len(removed), "file", path)
	return nil
}

func newTable() *tabwriter.Writer {
	return tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
}
