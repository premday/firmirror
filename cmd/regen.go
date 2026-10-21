package main

import (
	"context"

	"github.com/premday/firmirror/pkg/firmirror"
)

func runRegenMetadata(ctx context.Context) error {
	if err := requireTools("jcat-tool"); err != nil {
		return err
	}
	if err := checkSigningKeys(); err != nil {
		return err
	}
	definitions, err := loadDefinitions(ctx)
	if err != nil {
		return err
	}
	config := firmirror.FirmirrorConfig{
		Certificate:     args.Signature.Certificate,
		PrivateKey:      args.Signature.PrivateKey,
		FirmwareBaseURL: args.FirmwareBaseURL,
		BKC:             definitions,
	}
	args.RegenMetadata.apply(&config)
	syncer, cleanup, err := openSyncer(ctx, config)
	if err != nil {
		return err
	}
	defer cleanup()

	// The only thing left to do is to publish, which a signal during the load asks
	// not to.
	if err := ctx.Err(); err != nil {
		return err
	}
	endOnSecondSignal(ctx, cleanup)
	return syncer.RegenerateMetadata(ctx)
}
