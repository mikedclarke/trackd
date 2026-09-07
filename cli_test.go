package main

import (
	"encoding/json"
	"errors"
	"flag"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
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

// A mistyped flag is a mistake in the command line, so it exits 2 as a usage
// error like an unknown command already does, not 1, which scripts read as
// "trackd itself went wrong".
func TestUndefinedFlagIsAUsageError(t *testing.T) {
	cases := []struct {
		name string
		args []string
		n    int
	}{
		{"no positional", []string{"--frobnicate", "x"}, 0},
		{"with a key", []string{"TSK-1", "--frobnicate", "x"}, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fs := flag.NewFlagSet("issue list", flag.ContinueOnError)
			addCommon(fs)
			_, err := parseArgs(fs, tc.args, tc.n, "trackd issue list [flags]")
			var usage *usageError
			if !errors.As(err, &usage) {
				t.Fatalf("undefined flag = %v, want a usage error", err)
			}
			if !strings.Contains(err.Error(), "frobnicate") {
				t.Errorf("message = %q, want it to name the flag", err.Error())
			}
			// Both output shapes: the exit code a plain caller reads, and the
			// error object a --json caller parses off stdout.
			if got := exitCode(err); got != 2 {
				t.Errorf("exit code = %d, want 2", got)
			}
			obj := errorObject(err)
			if obj["code"] != "usage" || obj["exit"] != 2 {
				t.Errorf("--json error object = %v", obj)
			}
		})
	}

	// A help request stays a help request: main prints no error line for it.
	fs := flag.NewFlagSet("issue list", flag.ContinueOnError)
	addCommon(fs)
	err := parseFlags(fs, []string{"--help"}, "trackd issue list [flags]")
	if !errors.Is(err, flag.ErrHelp) || exitCode(err) != 2 {
		t.Errorf("--help = %v (exit %d), want flag.ErrHelp and exit 2", err, exitCode(err))
	}
}

// Replacing a description is refused to an agent token, and the CLI reports
// that refusal as an auth failure rather than something to retry.
func TestForbiddenExitsFour(t *testing.T) {
	err := &client.APIError{
		Status:  403,
		Code:    "forbidden",
		Message: "replacing a description needs an admin token; agents use append",
	}
	if got := exitCode(err); got != 4 {
		t.Errorf("exit code = %d, want 4", got)
	}
	obj := errorObject(err)
	if obj["code"] != "forbidden" || obj["exit"] != 4 || obj["status"] != 403 {
		t.Errorf("error object = %v", obj)
	}
}

func TestWantsJSON(t *testing.T) {
	cases := []struct {
		args []string
		want bool
	}{
		{[]string{"issue", "list"}, false},
		{[]string{"issue", "list", "--json"}, true},
		{[]string{"issue", "show", "-json", "TSK-1"}, true},
		{[]string{"issue", "list", "--json=true"}, true},
		{[]string{"issue", "comment", "TSK-1", "--body", "--json"}, true}, // conservative: a shape, not a value
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
	keys, err := parseArgs(fs, []string{"TSK-1", "--project", ""}, 1, "usage")
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
	_, err = parseArgs(fs, []string{"TSK-1", "--title", ""}, 1, "usage")
	if err == nil || err.Error() != "--title was given an empty value" {
		t.Errorf("empty --title = %v", err)
	}

	// The same rule applies to one element of a repeatable flag.
	fs, _ = newSet()
	_, err = parseArgs(fs, []string{"TSK-1", "--add-label", "seo", "--add-label", ""}, 1, "usage")
	if !errors.As(err, &usage) {
		t.Errorf("empty --add-label = %v, want a usage error", err)
	}

	// Clearing is explicit and always allowed.
	fs, _ = newSet()
	if _, err := parseArgs(fs, []string{"TSK-1", "--clear-project"}, 1, "usage"); err != nil {
		t.Errorf("--clear-project = %v, want no error", err)
	}
}

func TestFlagsMayPrecedeThePositionalKey(t *testing.T) {
	cases := []struct {
		name string
		args []string
	}{
		{"key first", []string{"TSK-1", "--status", "Done", "--json"}},
		{"key last", []string{"--status", "Done", "--json", "TSK-1"}},
		{"key in the middle", []string{"--json", "TSK-1", "--status", "Done"}},
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
			if len(keys) != 1 || keys[0] != "TSK-1" {
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
	for _, args := range [][]string{{}, {"TSK-1", "TSK-2"}} {
		_, err := parseArgs(fs, args, 1, "trackd issue show <key>")
		if exitCode(err) != 2 {
			t.Errorf("parseArgs(%v) = %v, want a usage error", args, err)
		}
	}
}

// The health report is a loose map, so the human table has to render a missing
// field rather than invent a zero for it.
func TestHealthRows(t *testing.T) {
	report := map[string]any{
		"status": "ok", "version": "abc1234", "schema": float64(3),
		"backup":    map[string]any{"last_at": "2026-09-06T07:31:47Z", "age_seconds": float64(90)},
		"integrity": map[string]any{"ok": true, "checked_at": "2026-09-06T07:31:47Z"},
	}
	if got := healthField(report, "schema"); got != "3" {
		t.Errorf("schema = %q, want 3", got)
	}
	if got := healthField(report, "nothing"); got != "-" {
		t.Errorf("missing field = %q, want a dash", got)
	}
	if got := healthBackup(healthSection(report, "backup")); got != "1m30s ago (2026-09-06T07:31:47Z)" {
		t.Errorf("backup row = %q", got)
	}
	if got := healthIntegrity(healthSection(report, "integrity")); got != "ok (checked 2026-09-06T07:31:47Z)" {
		t.Errorf("integrity row = %q", got)
	}

	degraded := map[string]any{"backup": map[string]any{}, "integrity": map[string]any{"ok": false}}
	if got := healthBackup(healthSection(degraded, "backup")); got != "no snapshot recorded" {
		t.Errorf("backup row without a snapshot = %q", got)
	}
	if got := healthIntegrity(healthSection(degraded, "integrity")); got != "failed (never checked)" {
		t.Errorf("integrity row without a check = %q", got)
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

// oneOf backs the --text/--body aliases: either name works, but naming the same
// thing twice is a usage error rather than a silent pick.
func TestOneOf(t *testing.T) {
	if v, err := oneOf("text", "hi", "body", ""); err != nil || v != "hi" {
		t.Errorf("primary only = %q, %v", v, err)
	}
	if v, err := oneOf("text", "", "body", "hi"); err != nil || v != "hi" {
		t.Errorf("alias only = %q, %v", v, err)
	}
	if v, err := oneOf("text", "", "body", ""); err != nil || v != "" {
		t.Errorf("neither = %q, %v (required is enforced by the caller)", v, err)
	}
	v, err := oneOf("text", "a", "body", "b")
	var usage *usageError
	if !errors.As(err, &usage) || v != "" {
		t.Errorf("both set = %q, %v, want a usage error", v, err)
	}
	if !strings.Contains(err.Error(), "text") || !strings.Contains(err.Error(), "body") {
		t.Errorf("both-set message = %q, want it to name both flags", err.Error())
	}
}

// `trackd comment GDL-1` is almost always someone reaching for the nested
// `issue comment`; the error points them there instead of a blank "unknown
// subcommand". A non-key subcommand still gets the plain error.
func TestCommentIssueKeyHint(t *testing.T) {
	err := cmdComment([]string{"GDL-1"})
	var usage *usageError
	if !errors.As(err, &usage) {
		t.Fatalf("comment GDL-1 = %v, want a usage error", err)
	}
	if !strings.Contains(err.Error(), "trackd issue comment GDL-1") {
		t.Errorf("hint = %q, want it to point at `trackd issue comment`", err.Error())
	}
	if exitCode(err) != 2 {
		t.Errorf("exit code = %d, want 2", exitCode(err))
	}

	err = cmdComment([]string{"nope"})
	if !errors.As(err, &usage) || !strings.Contains(err.Error(), "unknown comment subcommand") {
		t.Errorf("comment nope = %v, want the plain unknown-subcommand error", err)
	}
}

// A missing token is caught locally, before any request, and reported with the
// same exit 4 / "unauthorized" a server 401 would give.
func TestClientRequiresToken(t *testing.T) {
	t.Setenv("TRACKD_TOKEN", "")
	t.Setenv("TRACKD_URL", "")
	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	common := addCommon(fs)
	if _, err := parseArgs(fs, nil, 0, "t"); err != nil {
		t.Fatalf("parse: %v", err)
	}
	_, err := common.client()
	if err == nil {
		t.Fatal("client() with no token = nil error, want an auth error")
	}
	if exitCode(err) != 4 {
		t.Errorf("exit code = %d, want 4", exitCode(err))
	}
	obj := errorObject(err)
	if obj["code"] != "unauthorized" || obj["exit"] != 4 {
		t.Errorf("error object = %v", obj)
	}

	t.Setenv("TRACKD_TOKEN", "td_test")
	if _, err := common.client(); err != nil {
		t.Errorf("client() with a token = %v, want nil", err)
	}
}

// The admin/server commands parse through parseSet too, so an unknown flag on
// them is a usage error (exit 2), not the exit-1 "trackd broke" bucket.
func TestAdminUndefinedFlagIsUsage(t *testing.T) {
	err := cmdSetting([]string{"list", "--bogus"})
	var usage *usageError
	if !errors.As(err, &usage) {
		t.Fatalf("setting list --bogus = %v, want a usage error", err)
	}
	if exitCode(err) != 2 {
		t.Errorf("exit code = %d, want 2", exitCode(err))
	}
	if obj := errorObject(err); obj["code"] != "usage" || obj["exit"] != 2 {
		t.Errorf("error object = %v", obj)
	}
}

// captureStdout points os.Stdout at a pipe for the duration of fn and returns
// what it printed. The reader runs on its own goroutine so a long report
// cannot fill the pipe and deadlock the writer.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	saved := os.Stdout
	os.Stdout = w
	read := make(chan string, 1)
	go func() {
		b, _ := io.ReadAll(r)
		read <- string(b)
	}()
	fn()
	os.Stdout = saved
	w.Close()
	out := <-read
	r.Close()
	return out
}

// D9: removing a relation is idempotent. Whether there was one there or not,
// the command exits 0 and says which of the two happened, so a retry after a
// dropped connection does not read as a failure.
func TestIssueRelateRemoveIsIdempotent(t *testing.T) {
	cases := []struct {
		name    string
		removed bool
		want    string
	}{
		{"there", true, "removed TSK-1 blocks TSK-2"},
		{"already gone", false, "no TSK-1 blocks TSK-2 relation to remove"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if err := json.NewEncoder(w).Encode(map[string]any{
					"relations": []any{},
					"removed":   tc.removed,
				}); err != nil {
					t.Errorf("writing response: %v", err)
				}
			}))
			defer ts.Close()
			t.Setenv("TRACKD_URL", ts.URL)
			t.Setenv("TRACKD_TOKEN", "td_test")

			var err error
			out := captureStdout(t, func() {
				err = issueRelate([]string{"TSK-1", "TSK-2", "--type", "blocks", "--remove"})
			})
			if err != nil {
				t.Fatalf("relate --remove = %v, want no error", err)
			}
			if exitCode(err) != 0 {
				t.Errorf("exit code = %d, want 0", exitCode(err))
			}
			if !strings.Contains(out, tc.want) {
				t.Errorf("output = %q, want it to contain %q", out, tc.want)
			}
		})
	}
}

// D8: a command group asked for help prints its usage and exits 0, whichever
// of the three spellings was used. Called with no subcommand at all it prints
// the same line as a usage error (exit 2), because that is a command line
// missing its verb rather than a request for help.
func TestGroupUsage(t *testing.T) {
	const line = "usage: trackd token <add|list|revoke> [flags]"
	cases := []struct {
		name     string
		args     []string
		handled  bool
		wantExit int
	}{
		{"long help flag", []string{"--help"}, true, 0},
		{"short help flag", []string{"-h"}, true, 0},
		{"help word", []string{"help"}, true, 0},
		{"no subcommand", nil, true, 2},
		{"a real subcommand", []string{"list"}, false, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var handled bool
			var err error
			out := captureStdout(t, func() { handled, err = groupUsage(tc.args, line) })
			if handled != tc.handled {
				t.Fatalf("handled = %v, want %v", handled, tc.handled)
			}
			if got := exitCode(err); got != tc.wantExit {
				t.Errorf("exit code = %d, want %d (%v)", got, tc.wantExit, err)
			}
			if !tc.handled {
				return
			}
			// Either way the caller learns the subcommands: printed on stdout
			// for a help request, carried by the error otherwise.
			if tc.wantExit == 0 && !strings.Contains(out, line) {
				t.Errorf("stdout = %q, want the usage line", out)
			}
			if tc.wantExit != 0 && !strings.Contains(err.Error(), line) {
				t.Errorf("error = %q, want the usage line", err)
			}
		})
	}
}
