package types

import "time"

// SavedView is a named, reusable Log Explorer query: the LogQL text plus the
// time-range token the dashboard restores when the view is loaded. Views are
// cluster-scoped and shared (one list for the whole cluster, attributed via
// CreatedBy) — the first of the RuneSight dashboard resources (saved views,
// alert rules) persisted in the state store.
type SavedView struct {
	// Unique identifier.
	ID string `json:"id" yaml:"id"`

	// DNS-1123 cluster-unique name (doubles as the URL-safe slug).
	Name string `json:"name" yaml:"name"`

	// Description is optional free-text shown in the Saved Views list.
	Description string `json:"description,omitempty" yaml:"description,omitempty"`

	// LogQL is the saved query text (the Core-tier subset the ObserveService
	// parses). Stored verbatim; parsed/validated at save time so a view can
	// never hold an unparseable query.
	LogQL string `json:"logql" yaml:"logql"`

	// Range is the dashboard's relative time-range token ("15m", "1h", "24h",
	// ...). The Explorer resolves it to absolute start/end at load time so a
	// saved view is always "the last X", not a frozen window.
	Range string `json:"range,omitempty" yaml:"range,omitempty"`

	// Pinned views surface on the RuneSight home card and sort first.
	Pinned bool `json:"pinned,omitempty" yaml:"pinned,omitempty"`

	// CreatedBy attributes the view (auth principal name; informational).
	CreatedBy string `json:"createdBy,omitempty" yaml:"createdBy,omitempty"`

	CreatedAt time.Time `json:"createdAt" yaml:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt" yaml:"updatedAt"`
}
