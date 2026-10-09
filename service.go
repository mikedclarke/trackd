package main

import (
	"bytes"
	"encoding/xml"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"

	"github.com/mikedclarke/trackd/internal/client"
)

// The service group manages the launchd job that runs `trackd serve` on macOS:
// the commands an operator would otherwise type as launchctl lines. It only
// wraps launchctl, so it runs with exactly the rights of whoever calls it.

const serviceUsage = "usage: trackd service <status|start|stop|restart|install|uninstall> [--label <label>] [flags]"

// launchctl runs the real launchctl; tests replace it. The error carries
// launchctl's own words, which name the problem better than an exit status.
var launchctl = func(args ...string) (string, error) {
	out, err := exec.Command("launchctl", args...).CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if msg == "" {
			msg = err.Error()
		}
		return string(out), fmt.Errorf("launchctl %s: %s", strings.Join(args, " "), msg)
	}
	return string(out), nil
}

// serviceOS is runtime.GOOS; tests pin it so the suite runs on any platform.
var serviceOS = runtime.GOOS

// serviceLabelRe keeps the label to something that is safe as a file name,
// since the plist is written to <label>.plist.
var serviceLabelRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// launchdJob is one per-user launchd job: its label, the gui/<uid> domain it
// lives in and the plist that defines it.
type launchdJob struct {
	label  string
	domain string
	plist  string
}

func (j launchdJob) target() string { return j.domain + "/" + j.label }

// jobState is what `launchctl print` says about a loaded job.
type jobState struct {
	state          string
	pid            int
	runs           int
	lastExitReason string
}

// inspect reports whether the job is loaded and, when it is, its state.
func (j launchdJob) inspect() (bool, jobState, error) {
	out, err := launchctl("print", j.target())
	if err != nil {
		if strings.Contains(out, "Could not find service") {
			return false, jobState{}, nil
		}
		return false, jobState{}, err
	}
	return true, parseLaunchdPrint(out), nil
}

// parseLaunchdPrint reads the top-level fields of `launchctl print` output.
// Nested blocks (sockets, endpoints) carry their own "state" lines one tab
// deeper, so only lines indented by exactly one tab count.
func parseLaunchdPrint(out string) jobState {
	var st jobState
	for _, line := range strings.Split(out, "\n") {
		if !strings.HasPrefix(line, "\t") || strings.HasPrefix(line, "\t\t") {
			continue
		}
		key, value, ok := strings.Cut(strings.TrimSpace(line), " = ")
		if !ok {
			continue
		}
		switch key {
		case "state":
			st.state = value
		case "pid":
			st.pid, _ = strconv.Atoi(value)
		case "runs":
			st.runs, _ = strconv.Atoi(value)
		case "last exit reason":
			st.lastExitReason = value
		}
	}
	return st
}

func plistExists(path string) (bool, error) {
	_, err := os.Stat(path)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return false, err
}

func cmdService(args []string) error {
	if done, err := groupUsage(args, serviceUsage); done {
		return err
	}
	sub, rest := args[0], args[1:]
	fs := flag.NewFlagSet("service "+sub, flag.ContinueOnError)
	label := fs.String("label", os.Getenv("TRACKD_SERVICE_LABEL"), "launchd job label (default $TRACKD_SERVICE_LABEL)")
	var (
		url, db, addr, backupDir, logPath, bin *string
		jsonOut, dryRun                        *bool
	)
	switch sub {
	case "status":
		url = fs.String("url", "", "server URL to probe (default $TRACKD_URL, then http://127.0.0.1:8484)")
		jsonOut = fs.Bool("json", false, "output JSON instead of a line")
	case "install":
		db = fs.String("db", defaultDB(), "database path the server opens")
		addr = fs.String("addr", ":8484", "listen address")
		backupDir = fs.String("backup-dir", "", "snapshot directory; enables the backup scheduler")
		logPath = fs.String("log", "", "log file (default trackd.log next to the database)")
		bin = fs.String("bin", "", "trackd binary the job runs (default this binary)")
		dryRun = fs.Bool("dry-run", false, "print the plist and the launchctl line, change nothing")
	case "start", "stop", "restart", "uninstall":
	default:
		return usagef("unknown service subcommand %q", sub)
	}
	if err := parseSet(fs, rest); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return usagef("usage: trackd service %s [flags]", sub)
	}
	if *label == "" {
		return usagef("no launchd label: pass --label or set $TRACKD_SERVICE_LABEL (for example com.example.trackd)")
	}
	if !serviceLabelRe.MatchString(*label) {
		return usagef("launchd label %q may hold only letters, digits, dots, dashes and underscores", *label)
	}
	if serviceOS != "darwin" {
		return fmt.Errorf("trackd service manages a launchd job, so it runs on macOS only; on Linux use systemctl (README, Running as a service)")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	job := launchdJob{
		label:  *label,
		domain: fmt.Sprintf("gui/%d", os.Getuid()),
		plist:  filepath.Join(home, "Library", "LaunchAgents", *label+".plist"),
	}
	switch sub {
	case "status":
		return serviceStatus(job, *url, *jsonOut)
	case "start":
		return serviceStart(job)
	case "stop":
		return serviceStop(job)
	case "restart":
		return serviceRestart(job)
	case "install":
		return serviceInstall(job, *db, *addr, *backupDir, *logPath, *bin, *dryRun)
	default:
		return serviceUninstall(job)
	}
}

// statusReport is the --json form of `trackd service status`.
type statusReport struct {
	Label          string         `json:"label"`
	Plist          string         `json:"plist"`
	PlistExists    bool           `json:"plist_exists"`
	Loaded         bool           `json:"loaded"`
	State          string         `json:"state,omitempty"`
	PID            int            `json:"pid,omitempty"`
	Runs           int            `json:"runs,omitempty"`
	LastExitReason string         `json:"last_exit_reason,omitempty"`
	URL            string         `json:"url"`
	Health         map[string]any `json:"health,omitempty"`
	HealthError    string         `json:"health_error,omitempty"`
}

// serviceStatus puts launchd's view of the job and the server's own /healthz
// side by side, because either can be wrong while the other looks fine: a job
// launchd calls running can be stuck, and a healthy answer can come from a
// trackd started by hand. It exits 0 only when both agree all is well.
func serviceStatus(job launchdJob, url string, jsonOut bool) error {
	exists, err := plistExists(job.plist)
	if err != nil {
		return err
	}
	loaded, st, err := job.inspect()
	if err != nil {
		return err
	}
	if url == "" {
		url = os.Getenv("TRACKD_URL")
	}
	if url == "" {
		url = "http://127.0.0.1:8484"
	}
	r := statusReport{
		Label: job.label, Plist: job.plist, PlistExists: exists, Loaded: loaded,
		State: st.state, PID: st.pid, Runs: st.runs, LastExitReason: st.lastExitReason, URL: url,
	}
	// /healthz needs no token, and one attempt is the honest probe: a status
	// check that retried would hide a server that is flapping.
	health, code, herr := client.New(url, "").Health()
	if herr != nil {
		r.HealthError = herr.Error()
	} else {
		r.Health = health
	}
	if jsonOut {
		if err := printJSON(r); err != nil {
			return err
		}
	} else {
		fmt.Println(statusLine(r, code))
	}
	if r.State != "running" || herr != nil || code != 200 {
		return &client.APIError{Status: 503, Code: "service_down", Message: job.label + " is not running and healthy"}
	}
	return nil
}

func statusLine(r statusReport, healthCode int) string {
	var job string
	switch {
	case !r.Loaded && !r.PlistExists:
		job = "not installed (run: trackd service install)"
	case !r.Loaded:
		job = "not loaded (run: trackd service start)"
	case r.State == "running":
		job = fmt.Sprintf("running (pid %d, %d runs)", r.PID, r.Runs)
	default:
		job = r.State
		if r.LastExitReason != "" {
			job += ", last exit " + r.LastExitReason
		}
	}
	var server string
	switch {
	case r.HealthError != "":
		server = "server unreachable at " + r.URL
	default:
		status, _ := r.Health["status"].(string)
		if status == "" {
			status = fmt.Sprintf("answering %d", healthCode)
		}
		server = fmt.Sprintf("server %s, version %s at %s", status, healthField(r.Health, "version"), r.URL)
	}
	return fmt.Sprintf("%s: %s; %s", r.Label, job, server)
}

// serviceStart loads the job from its plist when launchd does not have it
// (after a stop), and kicks it when it is loaded but not running.
func serviceStart(job launchdJob) error {
	loaded, st, err := job.inspect()
	if err != nil {
		return err
	}
	if loaded {
		if st.state == "running" {
			fmt.Printf("%s is already running (pid %d)\n", job.label, st.pid)
			return nil
		}
		if _, err := launchctl("kickstart", job.target()); err != nil {
			return err
		}
		fmt.Println("started", job.label)
		return nil
	}
	exists, err := plistExists(job.plist)
	if err != nil {
		return err
	}
	if !exists {
		return fmt.Errorf("%s is not installed: no plist at %s (run: trackd service install)", job.label, job.plist)
	}
	if _, err := launchctl("bootstrap", job.domain, job.plist); err != nil {
		return err
	}
	fmt.Println("loaded and started", job.label)
	return nil
}

// serviceStop unloads the job. Killing the process would not do: a job with
// KeepAlive is respawned at once. The plist stays, so start brings it back.
func serviceStop(job launchdJob) error {
	loaded, _, err := job.inspect()
	if err != nil {
		return err
	}
	if !loaded {
		fmt.Println(job.label, "is not loaded")
		return nil
	}
	if _, err := launchctl("bootout", job.target()); err != nil {
		return err
	}
	fmt.Printf("stopped %s (unloaded; trackd service start loads it again)\n", job.label)
	return nil
}

// serviceRestart kills and respawns a loaded job; one that is not loaded is
// started instead, which is the full reload kickstart cannot do.
func serviceRestart(job launchdJob) error {
	loaded, _, err := job.inspect()
	if err != nil {
		return err
	}
	if !loaded {
		return serviceStart(job)
	}
	if _, err := launchctl("kickstart", "-k", job.target()); err != nil {
		return err
	}
	fmt.Println("restarted", job.label)
	return nil
}

// serviceInstall writes the plist and loads it. It never overwrites a plist
// that is already there: that file may carry settings written by hand, so the
// operator uninstalls first and means it.
func serviceInstall(job launchdJob, db, addr, backupDir, logPath, bin string, dryRun bool) error {
	exists, err := plistExists(job.plist)
	if err != nil {
		return err
	}
	if exists {
		return fmt.Errorf("%s is already installed at %s; run trackd service uninstall first to replace it", job.label, job.plist)
	}
	if bin == "" {
		if bin, err = os.Executable(); err != nil {
			return err
		}
	}
	// launchd runs the job from / with no shell, so every path is absolute,
	// and the binary is resolved past symlinks to the file that is executed.
	if bin, err = filepath.EvalSymlinks(bin); err != nil {
		return err
	}
	if bin, err = filepath.Abs(bin); err != nil {
		return err
	}
	if db, err = filepath.Abs(db); err != nil {
		return err
	}
	if logPath == "" {
		logPath = filepath.Join(filepath.Dir(db), "trackd.log")
	}
	if logPath, err = filepath.Abs(logPath); err != nil {
		return err
	}
	argv := []string{bin, "serve", "--db", db, "--addr", addr}
	if backupDir != "" {
		if backupDir, err = filepath.Abs(backupDir); err != nil {
			return err
		}
		argv = append(argv, "--backup-dir", backupDir)
	}
	plist := renderPlist(job.label, argv, logPath)
	if dryRun {
		fmt.Printf("dry run: would write %s:\n%s", job.plist, plist)
		fmt.Printf("dry run: would run: launchctl bootstrap %s %s\n", job.domain, job.plist)
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(job.plist), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(job.plist, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.WriteString(plist); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if _, err := launchctl("bootstrap", job.domain, job.plist); err != nil {
		return fmt.Errorf("wrote %s but could not load it: %w", job.plist, err)
	}
	fmt.Printf("installed and started %s (%s)\n", job.label, job.plist)
	return nil
}

// serviceUninstall unloads the job and removes its plist. The database, the
// log and the binary are left alone.
func serviceUninstall(job launchdJob) error {
	loaded, _, err := job.inspect()
	if err != nil {
		return err
	}
	exists, err := plistExists(job.plist)
	if err != nil {
		return err
	}
	if !loaded && !exists {
		fmt.Println(job.label, "is not installed")
		return nil
	}
	if loaded {
		if _, err := launchctl("bootout", job.target()); err != nil {
			return err
		}
	}
	if exists {
		if err := os.Remove(job.plist); err != nil {
			return err
		}
	}
	fmt.Printf("uninstalled %s (the database and log are untouched)\n", job.label)
	return nil
}

// renderPlist is the launchd job definition: run trackd serve, start it at
// load, and respawn it whenever it exits.
func renderPlist(label string, argv []string, logPath string) string {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
`)
	fmt.Fprintf(&b, "  <key>Label</key><string>%s</string>\n", xmlText(label))
	b.WriteString("  <key>ProgramArguments</key>\n  <array>\n")
	for _, a := range argv {
		fmt.Fprintf(&b, "    <string>%s</string>\n", xmlText(a))
	}
	b.WriteString("  </array>\n")
	b.WriteString("  <key>KeepAlive</key><true/>\n")
	b.WriteString("  <key>RunAtLoad</key><true/>\n")
	fmt.Fprintf(&b, "  <key>StandardOutPath</key><string>%s</string>\n", xmlText(logPath))
	fmt.Fprintf(&b, "  <key>StandardErrorPath</key><string>%s</string>\n", xmlText(logPath))
	b.WriteString("</dict>\n</plist>\n")
	return b.String()
}

func xmlText(s string) string {
	var buf bytes.Buffer
	_ = xml.EscapeText(&buf, []byte(s))
	return buf.String()
}
