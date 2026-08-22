package types

import "time"

// Release is the stateful record of what a `rune cast` installed. It is the
// source of truth for a runeset deployment: which resources it owns, the
// revision history, and the merged values used — enabling list, diff,
// upgrade-with-prune, rollback, and clean uninstall.
//
// Design: _docs/plugins/RUNESET_STATEFUL_RELEASES.md
//
// A Release is namespace-scoped (its "home" namespace) but its OwnerRefs may
// point into other namespaces (Decision D1). A release owns only the resources
// it created; inherently-shared cluster-scoped kinds (StorageClass, Namespace)
// are referenced, not owned (Decision D2).
type Release struct {
	// ID is a stable unique identifier for the release.
	ID string `json:"id" yaml:"id"`

	// Name is unique within the home namespace. Equals the --release value,
	// defaulting to the runeset manifest name, then the file/dir base name.
	Name string `json:"name" yaml:"name"`

	// Namespace is the release's home namespace.
	Namespace string `json:"namespace" yaml:"namespace"`

	// Status is the current lifecycle state of the release.
	Status ReleaseStatus `json:"status" yaml:"status"`

	// Revision increments on every successful cast of this release.
	Revision int `json:"revision" yaml:"revision"`

	// Source records where the runeset came from (dir, tgz, github, url) plus
	// the ref/digest, so a revision is reproducible.
	Source ReleaseSource `json:"source" yaml:"source"`

	// Manifest is the runeset.yaml that was deployed for this revision.
	Manifest RunesetManifest `json:"manifest" yaml:"manifest"`

	// Values is the fully-merged value set used to render this revision, kept
	// for diff and rollback. Never leaves the server on a normal read — see
	// HasValues and the RevealReleaseValues RPC.
	Values map[string]interface{} `json:"values,omitempty" yaml:"values,omitempty"`

	// HasValues reports whether the stored revision recorded a value set. It is
	// transport-only: the server derives it from Values when answering a read,
	// and a client uses it to decide whether it needs to reveal them. Not
	// persisted — the stored record has Values itself.
	HasValues bool `json:"-" yaml:"-"`

	// RenderedDigest is a checksum of the rendered castfile set, used as the
	// baseline for drift detection.
	RenderedDigest string `json:"renderedDigest,omitempty" yaml:"renderedDigest,omitempty"`

	// Owns is the authoritative set of resources this revision owns.
	Owns []OwnerRef `json:"owns,omitempty" yaml:"owns,omitempty"`

	// References records shared/cluster-scoped resources this release depends on
	// but does NOT own (and will never delete) — e.g. a pre-existing StorageClass
	// or Namespace it did not create (Decision D2).
	References []OwnerRef `json:"references,omitempty" yaml:"references,omitempty"`

	CreatedAt time.Time `json:"createdAt" yaml:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt" yaml:"updatedAt"`
}

// ReleaseStatus is the lifecycle state of a Release.
type ReleaseStatus string

const (
	// ReleaseStatusPending — intent recorded, apply in progress.
	ReleaseStatusPending ReleaseStatus = "pending"
	// ReleaseStatusDeployed — the current live revision.
	ReleaseStatusDeployed ReleaseStatus = "deployed"
	// ReleaseStatusSuperseded — a prior revision replaced by a newer one.
	ReleaseStatusSuperseded ReleaseStatus = "superseded"
	// ReleaseStatusFailed — an apply failed; prior deployed revision left live.
	ReleaseStatusFailed ReleaseStatus = "failed"
	// ReleaseStatusUninstalling — uninstall in progress.
	ReleaseStatusUninstalling ReleaseStatus = "uninstalling"
	// ReleaseStatusUninstalled — soft tombstone retained after uninstall (D4).
	ReleaseStatusUninstalled ReleaseStatus = "uninstalled"
)

// ReleaseSource records the provenance of a runeset deployment.
type ReleaseSource struct {
	// Type mirrors RunesetSourceType (directory, package-archive, github, ...).
	Type RunesetSourceType `json:"type" yaml:"type"`
	// Location is the raw source argument (path, URL, or github shorthand).
	Location string `json:"location" yaml:"location"`
	// Ref is the resolved version reference where applicable (git ref, tag).
	Ref string `json:"ref,omitempty" yaml:"ref,omitempty"`
	// Digest is the archive/content checksum where available.
	Digest string `json:"digest,omitempty" yaml:"digest,omitempty"`
}

// OwnerRef points to a single resource managed (or referenced) by a Release.
// The Namespace is carried per-ref so a release can span namespaces (D1).
type OwnerRef struct {
	ResourceType ResourceType `json:"resourceType" yaml:"resourceType"`
	Namespace    string       `json:"namespace" yaml:"namespace"`
	Name         string       `json:"name" yaml:"name"`
}

// OwnedBy is the system back-pointer stamped into an owned resource's metadata
// (NOT user labels), so GC and drift detection never depend on user edits.
type OwnedBy struct {
	// Release is the owning release name.
	Release string `json:"release" yaml:"release"`
	// Revision is the release revision that created or last touched the resource.
	Revision int `json:"revision" yaml:"revision"`
	// Manager identifies what manages the resource; "runeset" today.
	Manager string `json:"manager" yaml:"manager"`
}

// ManagerRuneset is the OwnedBy.Manager value for runeset-managed resources.
const ManagerRuneset = "runeset"

// Key returns the canonical "type/namespace/name" identity of a ref.
func (o OwnerRef) Key() string {
	return string(o.ResourceType) + "/" + o.Namespace + "/" + o.Name
}
