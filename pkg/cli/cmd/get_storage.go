// Package cmd — generic-verb (`rune get volume(s)`, `rune get storageclass(es)`)
// dispatch handlers for the storage subsystem.
//
// These functions delegate to the same renderers used by the noun-tree
// commands in volume.go and storageclass.go so the two CLI shapes always
// produce identical output.
package cmd

import (
	"fmt"
	"sort"

	"github.com/runestack/rune/pkg/api/client"
	"github.com/spf13/cobra"
)

// handleStorageClassGet handles `rune get storageclass[es] [<name>]`.
// StorageClasses are cluster-scoped, so namespace flags are ignored.
func handleStorageClassGet(_ *cobra.Command, opts *getOptions, resourceName string) error {
	api, err := createAPIClient(&opts.cmdOptions)
	if err != nil {
		return fmt.Errorf("failed to create API client: %w", err)
	}
	defer api.Close()
	scc := client.NewStorageClassClient(api)

	format := opts.outputFormat
	if format == "" {
		format = "table"
	}

	if resourceName != "" {
		sc, err := scc.GetStorageClass(resourceName)
		if err != nil {
			return fmt.Errorf("failed to get storage class %s: %w", resourceName, err)
		}
		return renderStorageClass(sc, format)
	}

	classes, err := scc.ListStorageClasses(parseLabelSelectorString(opts.labelSelector))
	if err != nil {
		return fmt.Errorf("failed to list storage classes: %w", err)
	}
	sort.Slice(classes, func(i, j int) bool { return classes[i].Name < classes[j].Name })
	if opts.limit > 0 && len(classes) > opts.limit {
		classes = classes[:opts.limit]
	}
	return renderStorageClasses(classes, format)
}

// handleVolumeGet handles `rune get volume(s) [<name>]`.
func handleVolumeGet(_ *cobra.Command, opts *getOptions, resourceName string) error {
	api, err := createAPIClient(&opts.cmdOptions)
	if err != nil {
		return fmt.Errorf("failed to create API client: %w", err)
	}
	defer api.Close()
	vc := client.NewVolumeClient(api)

	format := opts.outputFormat
	if format == "" {
		format = "table"
	}

	if resourceName != "" {
		ns := opts.namespace
		if ns == "" {
			ns = "default"
		}
		v, err := vc.GetVolume(ns, resourceName)
		if err != nil {
			return fmt.Errorf("failed to get volume %s: %w", resourceName, err)
		}
		return renderVolume(v, format)
	}

	target := opts.namespace
	if target == "" {
		target = "default"
	}
	if opts.allNamespaces {
		target = "*"
	}
	vols, err := vc.ListVolumes(
		target,
		parseLabelSelectorString(opts.labelSelector),
		parseLabelSelectorString(opts.fieldSelector),
	)
	if err != nil {
		return fmt.Errorf("failed to list volumes: %w", err)
	}
	sort.Slice(vols, func(i, j int) bool {
		if vols[i].Namespace != vols[j].Namespace {
			return vols[i].Namespace < vols[j].Namespace
		}
		return vols[i].Name < vols[j].Name
	})
	if opts.limit > 0 && len(vols) > opts.limit {
		vols = vols[:opts.limit]
	}
	return renderVolumes(vols, format, opts.allNamespaces)
}
