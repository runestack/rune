package types

import "testing"

// TestTopologySelectorMatches covers the AND semantics across MatchLabels
// and MatchExpressions plus each supported operator. Introduced as part
// of RUNE-073 Slice 7 (matcher utility; the scheduler-side consumer
// lands once a node registry exists).
func TestTopologySelectorMatches(t *testing.T) {
	cases := []struct {
		name     string
		selector TopologySelector
		labels   map[string]string
		want     bool
	}{
		{
			name:     "empty selector matches anything",
			selector: TopologySelector{},
			labels:   map[string]string{"any": "thing"},
			want:     true,
		},
		{
			name:     "empty selector matches nil labels",
			selector: TopologySelector{},
			labels:   nil,
			want:     true,
		},
		{
			name:     "matchLabels: single key value match",
			selector: TopologySelector{MatchLabels: map[string]string{TopologyLabelRegion: "nyc3"}},
			labels:   map[string]string{TopologyLabelRegion: "nyc3"},
			want:     true,
		},
		{
			name:     "matchLabels: value mismatch",
			selector: TopologySelector{MatchLabels: map[string]string{TopologyLabelRegion: "nyc3"}},
			labels:   map[string]string{TopologyLabelRegion: "fra1"},
			want:     false,
		},
		{
			name:     "matchLabels: missing key",
			selector: TopologySelector{MatchLabels: map[string]string{TopologyLabelRegion: "nyc3"}},
			labels:   map[string]string{TopologyLabelZone: "nyc3-1"},
			want:     false,
		},
		{
			name: "matchLabels: all pairs must match (AND)",
			selector: TopologySelector{MatchLabels: map[string]string{
				TopologyLabelRegion: "nyc3",
				TopologyLabelZone:   "nyc3-1",
			}},
			labels: map[string]string{TopologyLabelRegion: "nyc3"},
			want:   false,
		},
		{
			name: "In: value present in set",
			selector: TopologySelector{MatchExpressions: []TopologyMatchExpression{
				{Key: TopologyLabelZone, Operator: TopologyOperatorIn, Values: []string{"nyc3-1", "nyc3-2"}},
			}},
			labels: map[string]string{TopologyLabelZone: "nyc3-2"},
			want:   true,
		},
		{
			name: "In: value not in set",
			selector: TopologySelector{MatchExpressions: []TopologyMatchExpression{
				{Key: TopologyLabelZone, Operator: TopologyOperatorIn, Values: []string{"nyc3-1"}},
			}},
			labels: map[string]string{TopologyLabelZone: "nyc3-2"},
			want:   false,
		},
		{
			name: "In: key missing",
			selector: TopologySelector{MatchExpressions: []TopologyMatchExpression{
				{Key: TopologyLabelZone, Operator: TopologyOperatorIn, Values: []string{"nyc3-1"}},
			}},
			labels: map[string]string{TopologyLabelRegion: "nyc3"},
			want:   false,
		},
		{
			name: "NotIn: key absent matches",
			selector: TopologySelector{MatchExpressions: []TopologyMatchExpression{
				{Key: TopologyLabelZone, Operator: TopologyOperatorNotIn, Values: []string{"nyc3-1"}},
			}},
			labels: map[string]string{TopologyLabelRegion: "nyc3"},
			want:   true,
		},
		{
			name: "NotIn: value not in set matches",
			selector: TopologySelector{MatchExpressions: []TopologyMatchExpression{
				{Key: TopologyLabelZone, Operator: TopologyOperatorNotIn, Values: []string{"nyc3-1"}},
			}},
			labels: map[string]string{TopologyLabelZone: "nyc3-2"},
			want:   true,
		},
		{
			name: "NotIn: value in set fails",
			selector: TopologySelector{MatchExpressions: []TopologyMatchExpression{
				{Key: TopologyLabelZone, Operator: TopologyOperatorNotIn, Values: []string{"nyc3-1"}},
			}},
			labels: map[string]string{TopologyLabelZone: "nyc3-1"},
			want:   false,
		},
		{
			name: "Exists: key present (any value)",
			selector: TopologySelector{MatchExpressions: []TopologyMatchExpression{
				{Key: TopologyLabelHostPathRoot, Operator: TopologyOperatorExists},
			}},
			labels: map[string]string{TopologyLabelHostPathRoot: "/mnt/rune"},
			want:   true,
		},
		{
			name: "Exists: key absent fails",
			selector: TopologySelector{MatchExpressions: []TopologyMatchExpression{
				{Key: TopologyLabelHostPathRoot, Operator: TopologyOperatorExists},
			}},
			labels: map[string]string{TopologyLabelRegion: "nyc3"},
			want:   false,
		},
		{
			name: "DoesNotExist: key absent matches",
			selector: TopologySelector{MatchExpressions: []TopologyMatchExpression{
				{Key: TopologyLabelHostPathRoot, Operator: TopologyOperatorDoesNotExist},
			}},
			labels: map[string]string{TopologyLabelRegion: "nyc3"},
			want:   true,
		},
		{
			name: "DoesNotExist: key present fails",
			selector: TopologySelector{MatchExpressions: []TopologyMatchExpression{
				{Key: TopologyLabelHostPathRoot, Operator: TopologyOperatorDoesNotExist},
			}},
			labels: map[string]string{TopologyLabelHostPathRoot: "/mnt/rune"},
			want:   false,
		},
		{
			name: "matchLabels AND matchExpressions: both required",
			selector: TopologySelector{
				MatchLabels: map[string]string{TopologyLabelRegion: "nyc3"},
				MatchExpressions: []TopologyMatchExpression{
					{Key: TopologyLabelZone, Operator: TopologyOperatorIn, Values: []string{"nyc3-1"}},
				},
			},
			labels: map[string]string{
				TopologyLabelRegion: "nyc3",
				TopologyLabelZone:   "nyc3-1",
			},
			want: true,
		},
		{
			name: "matchLabels AND matchExpressions: expression failure fails whole",
			selector: TopologySelector{
				MatchLabels: map[string]string{TopologyLabelRegion: "nyc3"},
				MatchExpressions: []TopologyMatchExpression{
					{Key: TopologyLabelZone, Operator: TopologyOperatorIn, Values: []string{"nyc3-1"}},
				},
			},
			labels: map[string]string{
				TopologyLabelRegion: "nyc3",
				TopologyLabelZone:   "nyc3-2",
			},
			want: false,
		},
		{
			name: "unknown operator fails closed",
			selector: TopologySelector{MatchExpressions: []TopologyMatchExpression{
				{Key: TopologyLabelZone, Operator: TopologyOperator("Bogus"), Values: []string{"x"}},
			}},
			labels: map[string]string{TopologyLabelZone: "x"},
			want:   false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.selector.Matches(tc.labels); got != tc.want {
				t.Fatalf("Matches(%v) over selector %+v: got %v want %v", tc.labels, tc.selector, got, tc.want)
			}
		})
	}
}
