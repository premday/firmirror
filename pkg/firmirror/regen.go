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

// baseURLDropped is the refusal to regenerate the stored document without the base
// URL of its locations, which is what an operator who forgot --firmware-base-url
// would do, and which only the stored document tells. Force overrides it. The
// tags are held to what the publish checks.
func (f *FirmirrorSyncer) baseURLDropped() error {
	if f.facts.absolute && f.Config.FirmwareBaseURL == "" && !f.Config.Force {
		return errors.New("regenerating would take the base URL out of the locations of the stored metadata, as none is given (--firmware-base-url): pass it, or force it if that is the point")
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
	if err := f.baseURLDropped(); err != nil {
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
