package cmd

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/runestack/rune/pkg/api/client"
	"github.com/runestack/rune/pkg/cli/format"
	"github.com/runestack/rune/pkg/types"
	"github.com/runestack/rune/pkg/utils"
)

// runReleaseRollback rolls a release forward to a prior revision (D3/§4.Rollback,
// CAST_REFACTOR_PLAN §8.5). It re-renders the historical revision's stored
// Source + Values and casts it forward as a NEW revision (never mutating
// history).
//
// Because rendering is client-side (C1), rollback runs entirely in the CLI: it
// reads the target revision from History, re-materializes its source, re-renders
// with the stored values, and calls the Cast RPC. This only works for
// reproducible sources (tgz / github / url). A revision whose source was a local
// directory is no longer reproducible from the server's perspective and fails
// with a clear error.
func runReleaseRollback(namespace, name string, toRevision int, opts *castOptions) error {
	api, err := newAPIClient("", "")
	if err != nil {
		return err
	}
	defer api.Close()
	rcl := client.NewReleaseClient(api)

	// Load history and pick the target revision.
	revs, err := rcl.History(namespace, name)
	if err != nil {
		return err
	}
	if len(revs) == 0 {
		return fmt.Errorf("release %s/%s has no revision history", namespace, name)
	}

	target := pickRollbackTarget(revs, toRevision)
	if target == nil {
		return fmt.Errorf("revision %d not found in history of %s/%s", toRevision, namespace, name)
	}

	// The target source must be reproducible to re-render it. A local-directory
	// source is not (the directory may be gone or changed).
	rsrc, err := reproducibleSource(target.Source)
	if err != nil {
		return fmt.Errorf("cannot roll back %s/%s to revision %d: %w", namespace, name, target.Revision, err)
	}
	if rsrc.cleanup != nil {
		defer rsrc.cleanup()
	}

	// Re-render using the revision's stored values rather than re-merging from
	// disk, so a rollback reproduces exactly what was deployed.
	rollbackOpts := *opts
	rollbackOpts.releaseName = name
	rollbackOpts.namespace = namespace

	rendered, err := renderResolvedCast(rsrc, &rollbackOpts)
	if err != nil {
		return err
	}
	// Override the freshly-merged values with the stored revision's values so the
	// render is faithful to the historical deployment.
	if len(target.Values) > 0 {
		rendered.values = target.Values
	}

	if err := rendered.resolveSecretTemplates(api); err != nil {
		return err
	}

	spec := rendered.toReleaseSpec(name, target.Source, &rollbackOpts)

	fmt.Fprintf(os.Stderr, "Rolling %s/%s forward from revision %d as a new revision...\n", namespace, name, target.Revision)

	timeout, _ := time.ParseDuration(opts.timeoutStr)
	rel, _, err := rcl.Cast(spec, client.CastPayloads{
		Services:   rendered.payloads.services,
		Secrets:    rendered.payloads.secrets,
		Configmaps: rendered.payloads.configmaps,
		Volumes:    rendered.payloads.volumes,
	}, timeout)
	if err != nil {
		return err
	}
	fmt.Printf("%s Release %s/%s rolled back to the contents of revision %d (now revision %d)\n",
		format.Success("✓"), namespace, name, target.Revision, rel.Revision)
	return nil
}

// pickRollbackTarget selects the revision to roll back to. revision==0 means the
// immediately-preceding deployed revision (the second newest); otherwise the
// exact revision number. History is newest-first.
func pickRollbackTarget(revs []*types.Release, revision int) *types.Release {
	if revision > 0 {
		for _, r := range revs {
			if r.Revision == revision {
				return r
			}
		}
		return nil
	}
	// Default: the immediately-preceding revision (skip the current/newest).
	if len(revs) >= 2 {
		return revs[1]
	}
	return nil
}

// reproducibleSource turns a stored ReleaseSource into a resolvedCast by
// re-materializing reproducible sources (tgz / github / url). Local-directory
// sources are rejected as non-reproducible.
func reproducibleSource(src types.ReleaseSource) (*resolvedCast, error) {
	switch src.Type {
	case types.RunesetSourceTypeRemoteArchive:
		return resolveRemoteArchive(src.Location, &castOptions{})
	case types.RunesetSourceTypePackageArchive:
		// A package archive on disk: re-extract if it still exists.
		if !utils.FileExists(src.Location) {
			return nil, fmt.Errorf("package archive %q is no longer available", src.Location)
		}
		return resolvePackageArchive(src.Location, &castOptions{})
	case types.RunesetSourceTypeGitHub:
		loc := src.Location
		if src.Ref != "" && !strings.Contains(loc, "@") {
			loc = loc + "@" + src.Ref
		}
		return resolveGitHub(loc, &castOptions{})
	case types.RunesetSourceTypeDirectory:
		return nil, fmt.Errorf("the revision's source was a local directory (%q), which is not reproducible; "+
			"re-cast from the runeset source instead", src.Location)
	default:
		return nil, fmt.Errorf("unknown or unset source type %q; cannot reproduce", src.Type)
	}
}
