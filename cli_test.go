package main

import (
	"errors"
	"flag"
	"io"
	"os"
	"testing"

	"github.com/mikedclarke/trackd/internal/client"
)

func TestExitCode(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want int
	}{
		{"success", nil, 0},
		{"usage", usagef("bad flag"), 2},
		{"help", flag.ErrHelp, 2},
		{"validation", &client.APIError{Status: 400, Code: "validation"}, 2},
		{"invalid reference", &client.APIError{Status: 422, Code: "invalid_ref"}, 2},
		{"not found", &client.APIError{Status: 404, Code: "not_found"}, 3},
		{"unauthorized", &client.APIError{Status: 401, Code: "internal"}, 4},
		{"forbidden", &client.APIError{Status: 403, Code: "internal"}, 4},
		{"conflict", &client.APIError{Status: 409, Code: "conflict"}, 5},
		{"version conflict", &client.APIError{Status: 409, Code: "version_conflict"}, 5},
		{"busy", &client.APIError{Status: 503, Code: "busy"}, 6},
		{"server error", &client.APIError{Status: 500, Code: "internal"}, 6},
		{"unreachable", &client.TransportError{Op: "GET /", Err: errors.New("connection refused")}, 6},
		{"teapot", &client.APIError{Status: 418, Code: "internal"}, 1},
		{"anything else", errors.New("boom"), 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := exitCode(tc.err); got != tc.want {
				t.Errorf("exitCode(%v) = %d, want %d", tc.err, got, tc.want)
			}
		})
	}
}

func TestErrorObject(t *testing.T) {
	obj := errorObject(&client.APIError{Status: 409, Code: "description_replace", Message: "description replace refused"})
	if obj["code"] != "description_replace" || obj["status"] != 409 || obj["exit"] != 5 {
		t.Errorf("api error object = %v", obj)
	}
	if obj["error"] != "description replace refused" {
		t.Errorf("message = %v", obj["error"])
	}
	obj = errorObject(usagef("no such flag"))
	if obj["code"] != "usage" || obj["exit"] != 2 || obj["error"] != "no such flag" {
		t.Errorf("usage error object = %v", obj)
	}
	obj = errorObject(&client.TransportError{Op: "GET /", Err: errors.New("connection refused")})
	if obj["code"] != "unreachable" || obj["exit"] != 6 {
		t.Errorf("transport error object = %v", obj)
	}
}

func TestWantsJSON(t *testing.T) {
	cases := []struct {
		args []string
		want bool
	}{
		{[]string{"issue", "list"}, false},
		{[]string{"issue", "list", "--json"}, true},
		{[]string{"issue", "show", "-json", "GDL-1"}, true},
		{[]string{"issue", "list", "--json=true"}, true},
		{[]string{"issue", "comment", "GDL-1", "--body", "--json"}, true}, // conservative: a shape, not a value
		{[]string{"issue", "list", "--", "--json"}, false},
	}
	for _, tc := range cases {
		if got := wantsJSON(tc.args); got != tc.want {
			t.Errorf("wantsJSON(%v) = %v, want %v", tc.args, got, tc.want)
		}
	}
}

// withStdin points os.Stdin at a pipe holding text for the duration of fn.
func withStdin(t *testing.T, text string, fn func()) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	saved := os.Stdin
	os.Stdin = r
	defer func() { os.Stdin = saved; r.Close() }()
	if _, err := w.WriteString(text); err != nil {
		t.Fatal(err)
	}
	w.Close()
	fn()
}

func TestReadValue(t *testing.T) {
	got, err := readValue("description", "plain text")
	if err != nil || got != "plain text" {
		t.Errorf("literal value = %q, %v", got, err)
	}

	withStdin(t, "piped body\n", func() {
		got, err := readValue("description", "-")
		if err != nil || got != "piped body" {
			t.Errorf("stdin value = %q, %v", got, err)
		}
	})

	// An empty pipe is a command that produced nothing, never an instruction
	// to blank the field.
	for _, empty := range []string{"", "\n\n", "   \n"} {
		withStdin(t, empty, func() {
			_, err := readValue("body", "-")
			var usage *usageError
			if !errors.As(err, &usage) {
				t.Errorf("readValue with %q on stdin = %v, want a usage error", empty, err)
			}
			if exitCode(err) != 2 {
				t.Errorf("empty stdin exit code = %d, want 2", exitCode(err))
			}
		})
	}
}

func TestEmptyStringIsNeverAValue(t *testing.T) {
	newSet := func() (*flag.FlagSet, *stringSlice) {
		fs := flag.NewFlagSet("issue update", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		fs.String("project", "", "")
		fs.String("title", "", "")
		fs.Bool("clear-project", false, "")
		var labels stringSlice
		fs.Var(&labels, "add-label", "")
		return fs, &labels
	}

	fs, _ := newSet()
	keys, err := parseArgs(fs, []string{"GDL-1", "--project", ""}, 1, "usage")
	var usage *usageError
	if !errors.As(err, &usage) {
		t.Fatalf("empty --project = %v (keys %v), want a usage error", err, keys)
	}
	if exitCode(err) != 2 {
		t.Errorf("exit code = %d, want 2", exitCode(err))
	}
	if want := "--project was given an empty value; use --clear-project to clear the field"; err.Error() != want {
		t.Errorf("message = %q, want %q", err.Error(), want)
	}

	// A flag with no --clear- twin says so without inventing one.
	fs, _ = newSet()
	_, err = parseArgs(fs, []string{"GDL-1", "--title", ""}, 1, "usage")
	if err == nil || err.Error() != "--title was given an empty value" {
		t.Errorf("empty --title = %v", err)
	}

	// The same rule applies to one element of a repeatable flag.
	fs, _ = newSet()
	_, err = parseArgs(fs, []string{"GDL-1", "--add-label", "seo", "--add-label", ""}, 1, "usage")
	if !errors.As(err, &usage) {
		t.Errorf("empty --add-label = %v, want a usage error", err)
	}

	// Clearing is explicit and always allowed.
	fs, _ = newSet()
	if _, err := parseArgs(fs, []string{"GDL-1", "--clear-project"}, 1, "usage"); err != nil {
		t.Errorf("--clear-project = %v, want no error", err)
	}
}

func TestFlagsMayPrecedeThePositionalKey(t *testing.T) {
	cases := []struct {
		name string
		args []string
	}{
		{"key first", []string{"GDL-1", "--status", "Done", "--json"}},
		{"key last", []string{"--status", "Done", "--json", "GDL-1"}},
		{"key in the middle", []string{"--json", "GDL-1", "--status", "Done"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fs := flag.NewFlagSet("issue update", flag.ContinueOnError)
			fs.SetOutput(io.Discard)
			status := fs.String("status", "", "")
			jsonOut := fs.Bool("json", false, "")
			keys, err := parseArgs(fs, tc.args, 1, "trackd issue update <key>")
			if err != nil {
				t.Fatal(err)
			}
			if len(keys) != 1 || keys[0] != "GDL-1" {
				t.Errorf("keys = %v", keys)
			}
			if *status != "Done" || !*jsonOut {
				t.Errorf("status = %q, json = %v", *status, *jsonOut)
			}
		})
	}
}

func TestWrongNumberOfPositionalsIsAUsageError(t *testing.T) {
	fs := flag.NewFlagSet("issue show", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	addCommon(fs)
	for _, args := range [][]string{{}, {"GDL-1", "GDL-2"}} {
		_, err := parseArgs(fs, args, 1, "trackd issue show <key>")
		if exitCode(err) != 2 {
			t.Errorf("parseArgs(%v) = %v, want a usage error", args, err)
		}
	}
}

func TestSplitCSV(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"a,b", []string{"a", "b"}},
		{" a , b ", []string{"a", "b"}},
		{"a,,b", []string{"a", "b"}},
		{"", []string{}},
		{"  ", []string{}},
	}
	for _, tc := range cases {
		got := splitCSV(tc.in)
		if len(got) != len(tc.want) {
			t.Errorf("splitCSV(%q) = %v, want %v", tc.in, got, tc.want)
			continue
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Errorf("splitCSV(%q) = %v, want %v", tc.in, got, tc.want)
				break
			}
		}
	}
}
