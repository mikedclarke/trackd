package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

const runningPrint = `gui/501/com.example.trackd = {
	active count = 1
	path = /Users/alex/Library/LaunchAgents/com.example.trackd.plist
	type = LaunchAgent
	state = running
	program = /usr/local/bin/trackd
	runs = 3
	pid = 4242
	last exit reason = OS_REASON_CODESIGNING
	sockets = {
		state = active
	}
}
`

const waitingPrint = `gui/501/com.example.trackd = {
	state = not running
	runs = 7
	last exit code = 1
}
`

// fakeLaunchd stands in for launchctl: print answers from loaded and state,
// every other call is recorded so a test can check the exact lines run.
type fakeLaunchd struct {
	loaded bool
	print  string
	calls  []string
}

// stubService pins the platform, points HOME at a temp dir, sets the label
// through the environment and swaps launchctl for f. It returns the plist path
// the commands will use.
func stubService(t *testing.T, f *fakeLaunchd) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("TRACKD_SERVICE_LABEL", "com.example.trackd")
	savedOS, savedRun := serviceOS, launchctl
	t.Cleanup(func() { serviceOS, launchctl = savedOS, savedRun })
	serviceOS = "darwin"
	launchctl = func(args ...string) (string, error) {
		if args[0] == "print" {
			if !f.loaded {
				return "Bad request.\nCould not find service \"com.example.trackd\" in domain for user gui: 501\n", errors.New("exit status 113")
			}
			return f.print, nil
		}
		f.calls = append(f.calls, strings.Join(args, " "))
		return "", nil
	}
	return filepath.Join(home, "Library", "LaunchAgents", "com.example.trackd.plist")
}

func writePlist(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("<plist/>"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestParseLaunchdPrint(t *testing.T) {
	st := parseLaunchdPrint(runningPrint)
	// The nested socket's "state = active" must not overwrite the job's state.
	if st.state != "running" || st.pid != 4242 || st.runs != 3 || st.lastExitReason != "OS_REASON_CODESIGNING" {
		t.Errorf("parsed %+v", st)
	}
	st = parseLaunchdPrint(waitingPrint)
	if st.state != "not running" || st.pid != 0 || st.runs != 7 {
		t.Errorf("parsed %+v", st)
	}
}

func TestServiceLabel(t *testing.T) {
	cases := []struct {
		name string
		env  string
		args []string
		want int
	}{
		{"no label anywhere", "", []string{"service", "stop"}, 2},
		{"label with a slash", "", []string{"service", "stop", "--label", "../evil"}, 2},
		{"label from the environment", "com.example.trackd", []string{"service", "stop"}, 0},
		{"flag beats the environment", "bad/label", []string{"service", "stop", "--label", "com.example.other"}, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stubService(t, &fakeLaunchd{})
			t.Setenv("TRACKD_SERVICE_LABEL", tc.env)
			var err error
			captureStdout(t, func() { err = run(tc.args) })
			if got := exitCode(err); got != tc.want {
				t.Errorf("exit %d (%v), want %d", got, err, tc.want)
			}
		})
	}
}

func TestServiceRefusesOtherPlatforms(t *testing.T) {
	f := &fakeLaunchd{loaded: true, print: runningPrint}
	stubService(t, f)
	serviceOS = "linux"
	err := run([]string{"service", "restart"})
	if err == nil || !strings.Contains(err.Error(), "macOS only") {
		t.Fatalf("err = %v", err)
	}
	if len(f.calls) != 0 {
		t.Errorf("launchctl ran on linux: %v", f.calls)
	}
}

func TestServiceUnknownSubcommand(t *testing.T) {
	stubService(t, &fakeLaunchd{})
	if got := exitCode(run([]string{"service", "reload"})); got != 2 {
		t.Errorf("exit %d, want 2", got)
	}
}

func TestServiceLifecycle(t *testing.T) {
	cases := []struct {
		name      string
		sub       string
		loaded    bool
		print     string
		plist     bool
		wantCalls []string
		wantOut   string
		wantErr   string
	}{
		{"start a running job does nothing", "start", true, runningPrint, true, nil, "already running (pid 4242)", ""},
		{"start a loaded idle job kicks it", "start", true, waitingPrint, true, []string{"kickstart gui/%d/com.example.trackd"}, "started", ""},
		{"start an unloaded job loads the plist", "start", false, "", true, []string{"bootstrap gui/%d %p"}, "loaded and started", ""},
		{"start with no plist points at install", "start", false, "", false, nil, "", "not installed"},
		{"stop unloads", "stop", true, runningPrint, true, []string{"bootout gui/%d/com.example.trackd"}, "stopped", ""},
		{"stop an unloaded job is a no-op", "stop", false, "", true, nil, "is not loaded", ""},
		{"restart kills and respawns", "restart", true, runningPrint, true, []string{"kickstart -k gui/%d/com.example.trackd"}, "restarted", ""},
		{"restart an unloaded job starts it", "restart", false, "", true, []string{"bootstrap gui/%d %p"}, "loaded and started", ""},
		{"uninstall unloads and removes", "uninstall", true, runningPrint, true, []string{"bootout gui/%d/com.example.trackd"}, "uninstalled", ""},
		{"uninstall with nothing there", "uninstall", false, "", false, nil, "is not installed", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeLaunchd{loaded: tc.loaded, print: tc.print}
			plist := stubService(t, f)
			if tc.plist {
				writePlist(t, plist)
			}
			var err error
			out := captureStdout(t, func() { err = run([]string{"service", tc.sub}) })
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want %q", err, tc.wantErr)
				}
			} else if err != nil {
				t.Fatalf("err = %v", err)
			}
			if !strings.Contains(out, tc.wantOut) {
				t.Errorf("output %q, want %q", out, tc.wantOut)
			}
			var want []string
			for _, c := range tc.wantCalls {
				c = strings.ReplaceAll(c, "%d", strconv.Itoa(os.Getuid()))
				want = append(want, strings.ReplaceAll(c, "%p", plist))
			}
			if strings.Join(f.calls, "\n") != strings.Join(want, "\n") {
				t.Errorf("launchctl calls %q, want %q", f.calls, want)
			}
			if tc.sub == "uninstall" && tc.plist {
				if _, err := os.Stat(plist); !errors.Is(err, os.ErrNotExist) {
					t.Errorf("plist still there after uninstall: %v", err)
				}
			}
		})
	}
}

func TestServiceInstall(t *testing.T) {
	f := &fakeLaunchd{}
	plist := stubService(t, f)
	dir := t.TempDir()
	bin := filepath.Join(dir, "trackd")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "trackd-link")
	if err := os.Symlink(bin, link); err != nil {
		t.Fatal(err)
	}
	db := filepath.Join(dir, "a&b", "trackd.db")
	args := []string{"service", "install", "--bin", link, "--db", db, "--backup-dir", filepath.Join(dir, "backups")}

	// A dry run prints the plist and touches nothing.
	out := captureStdout(t, func() {
		if err := run(append(args, "--dry-run")); err != nil {
			t.Fatal(err)
		}
	})
	if !strings.Contains(out, "<key>Label</key><string>com.example.trackd</string>") {
		t.Errorf("dry run output:\n%s", out)
	}
	if _, err := os.Stat(plist); !errors.Is(err, os.ErrNotExist) || len(f.calls) != 0 {
		t.Fatalf("dry run changed something: stat %v, calls %v", err, f.calls)
	}

	captureStdout(t, func() {
		if err := run(args); err != nil {
			t.Fatal(err)
		}
	})
	b, err := os.ReadFile(plist)
	if err != nil {
		t.Fatal(err)
	}
	got := string(b)
	realBin, _ := filepath.EvalSymlinks(bin)
	for _, want := range []string{
		"<string>" + realBin + "</string>",
		"<string>serve</string>",
		"<string>" + filepath.Join(dir, "a&amp;b", "trackd.db") + "</string>",
		"<string>:8484</string>",
		"<string>" + filepath.Join(dir, "backups") + "</string>",
		"<key>StandardOutPath</key><string>" + filepath.Join(dir, "a&amp;b", "trackd.log") + "</string>",
		"<key>KeepAlive</key><true/>",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("plist lacks %q:\n%s", want, got)
		}
	}
	if len(f.calls) != 1 || f.calls[0] != "bootstrap gui/"+strconv.Itoa(os.Getuid())+" "+plist {
		t.Errorf("launchctl calls %v", f.calls)
	}

	// A second install never overwrites the plist.
	err = run(args)
	if err == nil || !strings.Contains(err.Error(), "already installed") {
		t.Fatalf("second install err = %v", err)
	}
	if b2, _ := os.ReadFile(plist); string(b2) != got {
		t.Error("second install changed the plist")
	}
}

func TestServiceStatus(t *testing.T) {
	healthy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/healthz" || r.Header.Get("Authorization") != "" {
			t.Errorf("probe %s with auth %q", r.URL.Path, r.Header.Get("Authorization"))
		}
		_, _ = w.Write([]byte(`{"status":"ok","version":"9.9.9"}`))
	}))
	defer healthy.Close()
	degraded := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"status":"degraded","version":"9.9.9"}`))
	}))
	defer degraded.Close()
	gone := httptest.NewServer(http.NotFoundHandler())
	gone.Close()

	cases := []struct {
		name     string
		loaded   bool
		print    string
		url      string
		wantExit int
		wantLine string
	}{
		{"running and healthy", true, runningPrint, healthy.URL, 0, "com.example.trackd: running (pid 4242, 3 runs); server ok, version 9.9.9"},
		{"running but degraded", true, runningPrint, degraded.URL, 6, "server degraded"},
		{"loaded but not running", true, waitingPrint, gone.URL, 6, "not running; server unreachable"},
		{"healthy server, job not loaded", false, "", healthy.URL, 6, "not loaded (run: trackd service start)"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			plist := stubService(t, &fakeLaunchd{loaded: tc.loaded, print: tc.print})
			writePlist(t, plist)
			var err error
			out := captureStdout(t, func() { err = run([]string{"service", "status", "--url", tc.url}) })
			if got := exitCode(err); got != tc.wantExit {
				t.Errorf("exit %d (%v), want %d", got, err, tc.wantExit)
			}
			if !strings.Contains(out, tc.wantLine) {
				t.Errorf("line %q, want %q", out, tc.wantLine)
			}
		})
	}

	t.Run("json", func(t *testing.T) {
		stubService(t, &fakeLaunchd{loaded: true, print: runningPrint})
		var err error
		out := captureStdout(t, func() { err = run([]string{"service", "status", "--json", "--url", healthy.URL}) })
		if err != nil {
			t.Fatal(err)
		}
		var r map[string]any
		if err := json.Unmarshal([]byte(out), &r); err != nil {
			t.Fatalf("%v in %s", err, out)
		}
		if r["state"] != "running" || r["pid"] != float64(4242) || r["plist_exists"] != false {
			t.Errorf("report %v", r)
		}
		if h, _ := r["health"].(map[string]any); h["status"] != "ok" {
			t.Errorf("health %v", r["health"])
		}
	})
}
