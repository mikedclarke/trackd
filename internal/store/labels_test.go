package store

import (
	"errors"
	"slices"
	"testing"
)

// S7/S8: label names resolve case-insensitively to the stored spelling, and
// nothing but the label endpoint creates one.
func TestLabelResolution(t *testing.T) {
	s := openTestStore(t)
	if _, err := s.EnsureLabel("Agent-Ready", "#f00"); err != nil {
		t.Fatal(err)
	}
	again, err := s.EnsureLabel("agent-ready", "")
	if err != nil {
		t.Fatal(err)
	}
	labels, err := s.ListLabels()
	if err != nil {
		t.Fatal(err)
	}
	if len(labels) != 1 || labels[0].ID != again.ID {
		t.Fatalf("case-different name made a second label: %+v", labels)
	}
	if labels[0].Name != "Agent-Ready" {
		t.Errorf("stored name = %q, want the first spelling", labels[0].Name)
	}
	issue := mustCreateIssue(t, s, IssueInput{Title: "Cased", Labels: []string{"AGENT-READY", "agent-ready"}}, "pm")
	if !slices.Equal(issue.Labels, []string{"Agent-Ready"}) {
		t.Errorf("labels = %v, want the canonical spelling once", issue.Labels)
	}
	if _, err := s.EnsureLabel("   ", ""); err == nil {
		t.Error("a blank label name was accepted")
	}
}

// S7: label ordering and blank handling.
func TestNormalizeLabels(t *testing.T) {
	s := openTestStore(t)
	mustLabel(t, s, "zulu", "alpha", "ops")
	issue := mustCreateIssue(t, s, IssueInput{Title: "Ordering", Labels: []string{"zulu", "", "  alpha  ", "zulu"}}, "")
	if !slices.Equal(issue.Labels, []string{"alpha", "zulu"}) {
		t.Errorf("labels = %v, want sorted and deduped", issue.Labels)
	}
	if _, err := s.UpdateIssue(issue.Key, IssuePatch{RemoveLabels: []string{"ghost"}}, ""); !errors.Is(err, ErrInvalidRef) {
		t.Errorf("removing an unknown label = %v, want ErrInvalidRef", err)
	}
}

// S7: an empty or malformed label_groups setting must not break label writes.
func TestLabelGroupsSetting(t *testing.T) {
	s := openTestStore(t)
	mustLabel(t, s, "ready", "blocked")
	if err := s.SetSetting("label_groups", ""); err != nil {
		t.Fatal(err)
	}
	issue := mustCreateIssue(t, s, IssueInput{Title: "No groups", Labels: []string{"ready", "blocked"}}, "")
	if len(issue.Labels) != 2 {
		t.Errorf("labels = %v, want both when no group is configured", issue.Labels)
	}
	if err := s.SetSetting("label_groups", "{not json"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.CreateIssue(IssueInput{Title: "Broken groups", Labels: []string{"ready"}}, ""); err == nil {
		t.Error("a malformed label_groups setting was ignored")
	}
}

// D5: a label color is a hex color or nothing. A value the board cannot
// render is a typo, and it is refused rather than stored.
func TestLabelColorValidation(t *testing.T) {
	cases := []struct {
		name  string
		color string
		want  bool
	}{
		{"none", "", true},
		{"short form", "#f00", true},
		{"long form", "#ff0000", true},
		{"upper case", "#FF00AA", true},
		{"mixed case", "#Ff00aA", true},
		{"no hash", "ff0000", false},
		{"a word", "red", false},
		{"too few digits", "#ff", false},
		{"too many digits", "#ff00aabb", false},
		{"not hex", "#gggggg", false},
		{"padded", " #ff0000 ", false},
		{"blank", "   ", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := openTestStore(t)
			label, err := s.EnsureLabel("ready", tc.color)
			if !tc.want {
				if !errors.Is(err, ErrInvalidRef) {
					t.Fatalf("color %q = %v, want ErrInvalidRef", tc.color, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("color %q = %v, want no error", tc.color, err)
			}
			if label.Color != tc.color {
				t.Errorf("stored color = %q, want %q", label.Color, tc.color)
			}
		})
	}

	// The same check guards a recolor of a label that already exists, which
	// is the other half of what EnsureLabel does.
	s := openTestStore(t)
	if _, err := s.EnsureLabel("ready", "#f00"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.EnsureLabel("ready", "puce"); !errors.Is(err, ErrInvalidRef) {
		t.Errorf("recolor with a bad value = %v, want ErrInvalidRef", err)
	}
	label, err := s.EnsureLabel("ready", "")
	if err != nil || label.Color != "#f00" {
		t.Errorf("label after a refused recolor = %+v, %v, want the old color kept", label, err)
	}
}
