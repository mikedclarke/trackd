package main

import (
	"encoding/json"
	"errors"
	"flag"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mikedclarke/trackd/internal/store"
)

func TestParseQuickAction(t *testing.T) {
	q, err := parseQuickAction(" Ship it : status=Done, priority=high, add=released, remove=waiting, remove=blocked ")
	if err != nil {
		t.Fatal(err)
	}
	if q.Name != "Ship it" || q.Status != "Done" || q.Priority == nil || *q.Priority != 2 ||
		strings.Join(q.AddLabels, ",") != "released" || strings.Join(q.RemoveLabels, ",") != "waiting,blocked" {
		t.Errorf("parsed = %+v", q)
	}
	for _, bad := range []string{"no colon", ": remove=x", "Name: remove", "Name: color=red", "Name: priority=vast"} {
		if _, err := parseQuickAction(bad); err == nil || exitCode(err) != 2 {
			t.Errorf("parseQuickAction(%q) = %v, want a usage error", bad, err)
		}
	}
}

func TestDescribeFilterRoundTrips(t *testing.T) {
	fs := newViewFlagSet()
	filterFlags := addViewFilterFlags(fs)
	args := []string{"--status", "Todo", "--status", "In Progress", "--label", "waiting", "--priority", "urgent", "--updated-within", "7d", "--order-by", "created"}
	if err := fs.Parse(args); err != nil {
		t.Fatal(err)
	}
	var filter viewFilter
	if err := filterFlags.apply(setFlags(fs), &filter); err != nil {
		t.Fatal(err)
	}
	want := `--status Todo --status "In Progress" --label waiting --priority urgent --updated-within 7d --order-by created`
	if got := describeFilter(filter); got != want {
		t.Errorf("describeFilter = %q, want %q", got, want)
	}
}

// view create posts the filter and quick actions the flags describe, and view
// update reads the stored view first so a single flag changes a single field.
func TestViewCreateAndUpdateBodies(t *testing.T) {
	var bodies []map[string]any
	var methods []string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		methods = append(methods, r.Method+" "+r.URL.Path)
		if r.Method != "GET" {
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decode body: %v", err)
			}
			bodies = append(bodies, body)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"name": "court", "owner": "pm", "shared": true,
			"filter":        map[string]any{"statuses": []string{"Todo"}, "labels": []string{"waiting"}, "order_by": "created"},
			"quick_actions": []any{},
		})
	}))
	defer ts.Close()
	t.Setenv("TRACKD_URL", ts.URL)
	t.Setenv("TRACKD_TOKEN", "td_test")

	captureStdout(t, func() {
		if err := cmdView([]string{"create", "court", "--label", "waiting", "--status", "Todo", "--private",
			"--quick", "Answered: remove=waiting", "--description", "parked on a person"}); err != nil {
			t.Fatalf("view create = %v", err)
		}
	})
	if len(bodies) != 1 || bodies[0]["shared"] != false || bodies[0]["description"] != "parked on a person" {
		t.Fatalf("create body = %v", bodies)
	}
	filter, _ := bodies[0]["filter"].(map[string]any)
	quick, _ := bodies[0]["quick_actions"].([]any)
	if labels, _ := filter["labels"].([]any); len(labels) != 1 || len(quick) != 1 {
		t.Errorf("create filter = %v, quick = %v", filter, quick)
	}

	// No filter flag at all is a usage error before any request.
	if err := cmdView([]string{"create", "empty"}); err == nil || exitCode(err) != 2 {
		t.Errorf("view create with no filter = %v, want usage error", err)
	}

	bodies, methods = nil, nil
	captureStdout(t, func() {
		if err := cmdView([]string{"update", "court", "--exclude-label", "ready", "--clear-order-by", "--shared"}); err != nil {
			t.Fatalf("view update = %v", err)
		}
	})
	if strings.Join(methods, ",") != "GET /api/v1/views/court,PATCH /api/v1/views/court" {
		t.Fatalf("update calls = %v", methods)
	}
	patch := bodies[0]
	filter, _ = patch["filter"].(map[string]any)
	if patch["shared"] != true || len(patch) != 2 {
		t.Errorf("patch body = %v", patch)
	}
	// The stored statuses and labels survive; the exclusion is added and the
	// order dropped.
	if statuses, _ := filter["statuses"].([]any); len(statuses) != 1 {
		t.Errorf("statuses lost on update: %v", filter)
	}
	if excluded, _ := filter["exclude_labels"].([]any); len(excluded) != 1 || filter["order_by"] != nil {
		t.Errorf("update filter = %v", filter)
	}

	// A no-op update is a usage error, and is caught after the read.
	err := cmdView([]string{"update", "court"})
	var usage *usageError
	if !errors.As(err, &usage) {
		t.Errorf("empty update = %v, want usage error", err)
	}

	bodies = nil
	out := captureStdout(t, func() {
		if err := cmdView([]string{"delete", "court"}); err != nil {
			t.Fatalf("view delete = %v", err)
		}
	})
	if bodies[0]["archived"] != true || !strings.Contains(out, "restore") {
		t.Errorf("delete body = %v, out = %q", bodies[0], out)
	}
	bodies = nil
	captureStdout(t, func() {
		if err := cmdView([]string{"restore", "court"}); err != nil {
			t.Fatalf("view restore = %v", err)
		}
	})
	if bodies[0]["archived"] != false {
		t.Errorf("restore body = %v", bodies[0])
	}
}

func TestIssueListViewAndPriorityFlags(t *testing.T) {
	var gotQuery string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"issues": []any{}, "next_offset": nil})
	}))
	defer ts.Close()
	t.Setenv("TRACKD_URL", ts.URL)
	t.Setenv("TRACKD_TOKEN", "td_test")
	captureStdout(t, func() {
		if err := issueList([]string{"--view", "court", "--priority", "urgent", "--priority", "2", "--created-by", "alex"}); err != nil {
			t.Fatalf("issue list = %v", err)
		}
	})
	for _, want := range []string{"view=court", "priority=1", "priority=2", "created_by=alex"} {
		if !strings.Contains(gotQuery, want) {
			t.Errorf("query %q missing %q", gotQuery, want)
		}
	}
}

// Small aliases so the round-trip test reads without the package prefix.
type viewFilter = store.ViewFilter

func newViewFlagSet() *flag.FlagSet { return flag.NewFlagSet("view", flag.ContinueOnError) }
