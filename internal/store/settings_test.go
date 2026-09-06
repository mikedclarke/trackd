package store

import "testing"

func TestValidateSetting(t *testing.T) {
	cases := []struct {
		key, value string
		ok         bool
	}{
		{"issue_prefix", "TSK", true},
		{"issue_prefix", "AB12", true},
		{"issue_prefix", "tsk", false},
		{"issue_prefix", "1AB", false},
		{"issue_prefix", "", false},
		{"issue_prefix", "TOOLONGPREFIX", false},
		{"label_groups", "", true},
		{"label_groups", `[["ready","blocked"]]`, true},
		{"label_groups", `[["ready","blocked"],["a","b","c"]]`, true},
		{"label_groups", `[["only-one"]]`, false},
		{"label_groups", `[["ready",""]]`, false},
		{"label_groups", `{not json`, false},
		{"label_groups", `["flat"]`, false},
		{"base_url", "", true},
		{"base_url", "https://trackd.example", true},
		{"base_url", "trackd.example", false},
		{"base_url", "ftp://x", false},
		{"issue_seq", "5", false},
		{"nope", "x", false},
	}
	for _, c := range cases {
		err := ValidateSetting(c.key, c.value)
		if (err == nil) != c.ok {
			t.Errorf("ValidateSetting(%q, %q) = %v, want ok=%v", c.key, c.value, err, c.ok)
		}
	}
}

func TestListSettings(t *testing.T) {
	s := openTestStore(t)
	list, err := s.ListSettings()
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]SettingValue{}
	for _, v := range list {
		got[v.Key] = v
	}
	if got["issue_prefix"].Value != "TSK" {
		t.Errorf("default issue_prefix = %q, want TSK", got["issue_prefix"].Value)
	}
	if got["label_groups"].Value != "" {
		t.Errorf("default label_groups = %q, want empty", got["label_groups"].Value)
	}
	if !got["issue_seq"].ReadOnly {
		t.Error("issue_seq should be read-only")
	}
	if err := s.SetSetting("issue_prefix", "ACME"); err != nil {
		t.Fatal(err)
	}
	list, _ = s.ListSettings()
	for _, v := range list {
		if v.Key == "issue_prefix" && v.Value != "ACME" {
			t.Errorf("issue_prefix after set = %q", v.Value)
		}
	}
}
