package main

import (
	"context"
	"errors"
	"fmt"

	"github.com/jonasohland/mxl-replicator/internal/api"
	"github.com/jonasohland/mxl-replicator/internal/client"
	"github.com/jonasohland/mxl-replicator/internal/manifest"
)

// DeleteCmd is `mxl-replicator delete`, by manifest or by name.
//
//	mxl-replicator delete -f studio-a.yaml
//	mxl-replicator delete -n nab cam1-distribution talkback
//
// Cancelling a request is the only thing that removes one: the system never cancels one on the
// user's behalf because a session is failing (§11). The path underneath survives until the last
// request referencing it is gone, which is what refcounting is for.
//
// `-f` takes only the **kinds and names** from the file and ignores everything else in it, so the
// manifest that created a set removes it without having to still be accurate about where anything
// goes. Documents are processed in reverse apply order — requests, then the namespaces they live
// in — because a namespace delete is refused while any request references it (§9.3).
//
// Output is a list of `<id> <what happened>`, matching `apply` — see the note there for why
// neither verb prints a table.
type DeleteCmd struct {
	Files     []string `short:"f" help:"Manifest file, directory of *.yaml, or - for stdin. Only the kinds and names are read. Repeatable."`
	Namespace string   `short:"n" help:"Namespace the named requests are in. Defaults to 'default'."`
	Names     []string `arg:"" optional:"" help:"Request names to cancel, within --namespace."`

	ClientFlags `embed:""`
}

func (c *DeleteCmd) Run(ctx context.Context) error {
	ns := c.Namespace
	if ns == "" {
		ns = api.DefaultNamespace
	}

	var requests []api.RequestID
	var namespaces []string

	for _, name := range c.Names {
		requests = append(requests, api.RequestID{Namespace: ns, Name: name})
	}

	if len(c.Files) > 0 {
		// **Names only.** A document carrying nothing but `kind:` and `name:` is enough, and a
		// manifest that has drifted from what is deployed still deletes what it named — which is
		// the point, since "remove what this file created" is wanted most exactly when the file is
		// no longer an accurate description of anything.
		docs, err := manifest.Load(c.Files)
		if err != nil {
			return err
		}
		for _, doc := range manifest.SortForDelete(docs) {
			switch doc.Kind {
			case manifest.KindNamespace:
				namespaces = append(namespaces, doc.Name())
			case manifest.KindDomain:
				// **Deliberately skipped.** A label is a fact about a host rather than intent, and
				// `--prune` does not reach one either (§9.1). Removing labels is `label key-`, or
				// an apply that no longer declares the key — both of which say what they mean;
				// "delete this file" does not.
			default:
				requests = append(requests, doc.ID())
			}
		}
	}

	if len(requests) == 0 && len(namespaces) == 0 {
		return errors.New("nothing to delete: name a request, or pass -f <manifest>")
	}

	cl, err := c.client()
	if err != nil {
		return err
	}

	total := len(requests) + len(namespaces)
	failed := 0
	for _, id := range requests {
		failed += deleteOne(id.String(), func() (bool, error) { return cl.DeleteRequest(ctx, id) })
	}
	// After the requests, so a file that declares a namespace and the requests in it can remove
	// both in one pass. A namespace still holding requests this file did not name is refused by the
	// server, with the count, which is the answer an operator needs rather than a cascade.
	for _, name := range namespaces {
		failed += deleteOne("namespace/"+name, func() (bool, error) { return cl.DeleteNamespace(ctx, name) })
	}

	if failed > 0 {
		return fmt.Errorf("%d of %d objects could not be deleted", failed, total)
	}
	return nil
}

// deleteOne runs one delete and prints its line. Returns 1 if it failed.
//
// An object that is not there is not a failure. Deleting what a manifest names is idempotent by
// nature, and the second run of a delete should succeed rather than fail because the first one
// worked.
func deleteOne(label string, delete func() (bool, error)) int {
	found, err := delete()
	switch {
	case err != nil:
		fmt.Printf("%s error: %s\n", label, deleteError(err))
		return 1
	case found:
		fmt.Printf("%s cancelled\n", label)
	default:
		fmt.Printf("%s not found\n", label)
	}
	return 0
}

// deleteError renders a refusal compactly, keeping the server's message — which for a namespace
// still in use carries the count of requests to cancel first.
func deleteError(err error) string {
	var apiErr *client.APIError
	if errors.As(err, &apiErr) {
		return apiErr.Body.Message
	}
	return err.Error()
}
