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
