package store

import (
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"strings"
)

// SettingSpec describes one operator-facing setting: the key, what it does,
// and whether the setting command may write it.
type SettingSpec struct {
	Key         string
	Description string
	ReadOnly    bool
}

// SettingSpecs is the list of settings an operator can see through
// `trackd setting`, in display order. Anything else in the table is internal.
var SettingSpecs = []SettingSpec{
	{Key: "issue_prefix", Description: "prefix for new issue keys, e.g. TSK gives TSK-1; existing keys keep theirs"},
	{Key: "issue_seq", Description: "the last issue number handed out (read-only)", ReadOnly: true},
	{Key: "label_groups", Description: `JSON array of label groups, e.g. [["ready","blocked"]]; an issue holds at most one label from each group`},
	{Key: "base_url", Description: "public URL of the server, used to fill each issue's url field; empty leaves it out"},
}

// SettingValue is one row of `trackd setting list`.
type SettingValue struct {
	Key         string `json:"key"`
	Value       string `json:"value"`
	Description string `json:"description"`
	ReadOnly    bool   `json:"read_only,omitempty"`
}

var issuePrefixPattern = regexp.MustCompile(`^[A-Z][A-Z0-9]{0,9}$`)

// ValidateSetting checks a value an operator wants to store. Only the known,
// writable keys pass; the value rules keep the read paths that trust these
// settings from failing later on a bad one.
func ValidateSetting(key, value string) error {
	var spec *SettingSpec
	for i := range SettingSpecs {
		if SettingSpecs[i].Key == key {
			spec = &SettingSpecs[i]
		}
	}
	if spec == nil {
		return fmt.Errorf("unknown setting %q (run: trackd setting list)", key)
	}
	if spec.ReadOnly {
		return fmt.Errorf("setting %q is read-only", key)
	}
	switch key {
	case "issue_prefix":
		if !issuePrefixPattern.MatchString(value) {
			return fmt.Errorf("issue_prefix must be 1-10 uppercase letters or digits, starting with a letter, got %q", value)
		}
	case "label_groups":
		if strings.TrimSpace(value) == "" {
			return nil
		}
		var groups [][]string
		if err := json.Unmarshal([]byte(value), &groups); err != nil {
			return fmt.Errorf("label_groups must be a JSON array of arrays of label names: %v", err)
		}
		for i, g := range groups {
			if len(g) < 2 {
				return fmt.Errorf("label_groups: group %d needs at least two labels", i+1)
			}
			for _, name := range g {
				if strings.TrimSpace(name) == "" {
					return fmt.Errorf("label_groups: group %d holds an empty label name", i+1)
				}
			}
		}
	case "base_url":
		if value == "" {
			return nil
		}
		u, err := url.Parse(value)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return fmt.Errorf("base_url must be an http or https URL, got %q", value)
		}
	}
	return nil
}

// ListSettings returns the operator-facing settings with their current values.
func (s *Store) ListSettings() ([]SettingValue, error) {
	out := make([]SettingValue, 0, len(SettingSpecs))
	for _, spec := range SettingSpecs {
		v, err := s.Setting(spec.Key)
		if err != nil {
			return nil, err
		}
		if spec.Key == "issue_prefix" && v == "" {
			v = "TSK"
		}
		out = append(out, SettingValue{Key: spec.Key, Value: v, Description: spec.Description, ReadOnly: spec.ReadOnly})
	}
	return out, nil
}
