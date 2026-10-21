package firmirror

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/premday/firmirror/pkg/lvfs"
	"github.com/premday/firmirror/pkg/utils"
)

// ReadMetadata reads published metadata from a path or an HTTP(S) URL, as a
// client does, with no storage behind it. With strict, plain HTTP is refused, and
// so is a redirect from HTTPS to it.
func ReadMetadata(ctx context.Context, source string, strict bool) ([]lvfs.Component, error) {
	compressed, err := utils.ReadSource(ctx, source, maxDocumentSize, strict)
	if err != nil {
		return nil, fmt.Errorf("reading the metadata: %w", err)
	}
	shown := utils.Redacted(source)
	components, _, err := decodeStored(compressed)
	if err != nil {
		return nil, fmt.Errorf("reading the metadata from %s: %w", shown, err)
	}
	return componentsOf(components, "the metadata from "+shown, "its components are not built the way this firmirror identifies them")
}

// baseURLDropped refuses to publish bare locations over a stored document whose
// locations are URLs, which is what forgetting --firmware-base-url does.
// DropBaseURL overrides it, Force does not.
func (f *FirmirrorSyncer) baseURLDropped() error {
	if f.facts.absolute && f.Config.FirmwareBaseURL == "" && !f.Config.DropBaseURL {
		return errors.New("the stored metadata names its packages by URL under a base URL, and none is given (--firmware-base-url), " +
			"which would name them by bare file name, as fwupd remotes that set FirmwareBaseURI reject: pass it, or drop it if that is the point")
	}
	return nil
}

// RegenerateMetadata publishes the stored document again, rendered with the
// current definitions and signed with the current keys, even if the document is
// unchanged: a refresh leaves an unchanged document alone, so it would not pick up
// a new signing key.
func (f *FirmirrorSyncer) RegenerateMetadata(ctx context.Context) error {
	ctx = context.WithoutCancel(ctx)
	if err := f.noMetadata(); err != nil {
		return err
	}
	document, _ := f.buildDocument(false)
	outcome, err := f.writeMetadata(ctx, document, true)
	if err != nil {
		return err
	}
	if outcome == metadataRepublished {
		return errors.New("the metadata was published while it was regenerated, which would have replaced it with an older one: run it again")
	}
	slog.With("component", "metadata-regen").Info("Metadata regenerated", "components", len(document.Component))
	return nil
}

// componentsOf returns the whole components of a parsed document, or an error
// when it is not of the schema version this firmirror publishes.
func componentsOf(components *lvfs.Components, what, hint string) ([]lvfs.Component, error) {
	if components.SchemaVersion != lvfs.MetadataSchemaVersion {
		return nil, schemaError(what, components.SchemaVersion, hint)
	}
	return lvfs.PoolByKey(components.Component), nil
}

// ReadStoredComponents reads the whole components of the stored metadata, for a
// command that only needs to know what is mirrored.
func ReadStoredComponents(ctx context.Context, storage Storage) ([]lvfs.Component, error) {
	compressed, exists, err := readStoredFile(ctx, storage)
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, errors.New("there is no metadata: run a refresh first")
	}
	components, _, err := decodeStored(compressed)
	if err != nil {
		return nil, err
	}
	return componentsOf(components, "the stored metadata", "run a refresh, which mirrors everything again")
}
