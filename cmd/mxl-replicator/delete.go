package main

import (
	"context"
	"errors"
	"fmt"

	"github.com/jonasohland/mxl-replicator/internal/manifest"
)

// DeleteCmd is `mxl-replicator delete`, by manifest or by name.
//
//	mxl-replicator delete -f studio-a.yaml
//	mxl-replicator delete cam1-distribution talkback
//
// Cancelling a request is the only thing that removes one: the system never cancels one on the
// user's behalf because a session is failing (§11). The path underneath survives until the last
// request referencing it is gone, which is what refcounting is for.
//
// `-f` takes only the **names** from the file and ignores everything else in it, so the manifest
// that created a set removes it without having to still be accurate about where anything goes.
type DeleteCmd struct {
	Files []string `short:"f" help:"Manifest file, directory of *.yaml, or - for stdin. Only the names are read. Repeatable."`
	Names []string `arg:"" optional:"" help:"Request names to cancel."`

	ClientFlags `embed:""`
}

func (c *DeleteCmd) Run(ctx context.Context) error {
	names := c.Names
	if len(c.Files) > 0 {
		// **Names only.** A document carrying nothing but `name:` is enough, and a manifest that
		// has drifted from what is deployed still deletes what it named — which is the point, since
		// "remove what this file created" is wanted most exactly when the file is no longer an
		// accurate description of anything.
		docs, err := manifest.Load(c.Files)
		if err != nil {
			return err
		}
		names = append(names, manifest.Names(docs)...)
	}
	if len(names) == 0 {
		return errors.New("nothing to delete: name a request, or pass -f <manifest>")
	}

	api, err := c.client()
	if err != nil {
		return err
	}

	var failed int
	for _, name := range names {
		// A request that is not there is not a failure. Deleting what a manifest names is
		// idempotent by nature, and the second run of a delete should succeed rather than fail
		// because the first one worked.
		found, err := api.DeleteRequest(ctx, name)
		switch {
		case err != nil:
			fmt.Printf("%-40s error: %s\n", name, err)
			failed++
		case found:
			fmt.Printf("%-40s cancelled\n", name)
		default:
			fmt.Printf("%-40s not found\n", name)
		}
	}

	if failed > 0 {
		return fmt.Errorf("%d of %d requests could not be cancelled", failed, len(names))
	}
	return nil
}
