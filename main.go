package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/mikedclarke/trackd/internal/server"
	"github.com/mikedclarke/trackd/internal/store"
)

var version = "dev"

func main() {
	err := run(os.Args[1:])
	if err == nil {
		return
	}
	// --json callers parse stdout, so the machine-readable error goes there and
	// the human line to stderr. The flag package has already printed its own
	// usage for a help request, so that one is not repeated.
	if wantsJSON(os.Args[1:]) {
		_ = printJSON(errorObject(err))
	}
	if !errors.Is(err, flag.ErrHelp) {
		fmt.Fprintln(os.Stderr, "trackd:", err)
	}
	os.Exit(exitCode(err))
}

func run(args []string) error {
	if len(args) == 0 {
		usage()
		return nil
	}
	cmd, rest := args[0], args[1:]
	switch cmd {
	case "version":
		fmt.Println("trackd", version)
		return nil
	case "serve":
		return cmdServe(rest)
	case "issue":
		return cmdIssue(rest)
	case "comment":
		return cmdComment(rest)
	case "events":
		return cmdEvents(rest)
	case "project":
		return cmdProject(rest)
	case "label":
		return cmdLabel(rest)
	case "milestone":
		return cmdMilestone(rest)
	case "statuses":
		return cmdStatuses(rest)
	case "health":
		return cmdHealth(rest)
	case "token":
		return cmdToken(rest)
	case "setting":
		return cmdSetting(rest)
	case "backup":
		return cmdBackup(rest)
	case "restore":
		return cmdRestore(rest)
	case "export":
		return cmdExport(rest)
	case "import":
		return cmdImport(rest)
	case "help", "-h", "--help":
		// "trackd help issue update" is "trackd issue update --help".
		if len(rest) > 0 {
			return run(append(rest, "--help"))
		}
		usage()
		return nil
	default:
		usage()
		return usagef("unknown command %q", cmd)
	}
}

func defaultDB() string {
	if v := os.Getenv("TRACKD_DB"); v != "" {
		return v
	}
	return "trackd.db"
}

// openError turns the two open failures an operator can act on into plain
// advice instead of a wrapped sentinel.
func openError(path string, err error) error {
	switch {
	case errors.Is(err, store.ErrIntegrity):
		return fmt.Errorf("%s failed its integrity check and was not opened; restore the newest verified snapshot into a new file with: trackd restore --db <new path> <snapshot>", path)
	case errors.Is(err, store.ErrSchemaNewer):
		return fmt.Errorf("%s was written by a newer trackd; upgrade this binary before opening the file", path)
	}
	return err
}

// openLocked opens the database read-write and takes the exclusive lock, so a
// command that writes to the file can never run behind a live server's back.
func openLocked(path string) (*store.Store, func(), error) {
	st, err := store.Open(path)
	if err != nil {
		return nil, nil, openError(path, err)
	}
	release, err := st.LockExclusive()
	if err != nil {
		st.Close()
		if errors.Is(err, store.ErrLocked) {
			return nil, nil, fmt.Errorf("another trackd is running on %s", path)
		}
		return nil, nil, err
	}
	return st, release, nil
}

func cmdServe(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	db := fs.String("db", defaultDB(), "database path")
	addr := fs.String("addr", ":8484", "listen address")
	backupDir := fs.String("backup-dir", "", "snapshot directory; enables the backup scheduler")
	backupEvery := fs.Duration("backup-every", 24*time.Hour, "interval between scheduled backups")
	backupKeep := fs.Int("backup-keep", 14, "scheduled backups to retain (0 = never prune)")
	backupTimeout := fs.Duration("backup-timeout", 10*time.Minute, "abandon a backup run that exceeds this")
	if err := fs.Parse(args); err != nil {
		return err
	}
	// The pre-migration snapshot belongs with the other backups when there is
	// a backup directory: it is the copy you want if a migration goes wrong.
	st, err := store.OpenWith(*db, store.Options{SnapshotDir: *backupDir})
	if err != nil {
		return openError(*db, err)
	}
	defer st.Close()
	release, err := st.LockExclusive()
	if err != nil {
		if errors.Is(err, store.ErrLocked) {
			return fmt.Errorf("another trackd is running on %s", *db)
		}
		return err
	}
	defer release()

	tokens, err := st.ListTokens()
	if err != nil {
		return err
	}
	if len(tokens) == 0 {
		plaintext, err := st.CreateToken("admin", "admin")
		if err != nil {
			return err
		}
		// stderr, so redirecting stdout to a log file does not put a live
		// credential in it.
		fmt.Fprintf(os.Stderr, "created initial admin token (store it now; it is never shown again):\n  %s\n", plaintext)
	}

	srv := server.New(st, version)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go srv.RunBackups(ctx, server.BackupConfig{Dir: *backupDir, Every: *backupEvery, Keep: *backupKeep, Timeout: *backupTimeout})

	httpSrv := &http.Server{
		Addr:              *addr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		WriteTimeout:      server.WriteTimeout,
		IdleTimeout:       server.IdleTimeout,
	}
	shutdownDone := make(chan struct{})
	go func() {
		defer close(shutdownDone)
		<-ctx.Done()
		log.Print("shutting down")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), server.ShutdownBudget)
		defer cancel()
		if err := httpSrv.Shutdown(shutdownCtx); err != nil {
			log.Printf("shutdown: %v", err)
		}
	}()
	log.Printf("trackd %s listening on %s (db %s)", version, *addr, *db)
	if err := httpSrv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	<-shutdownDone
	return nil
}

func cmdToken(args []string) error {
	if done, err := groupUsage(args, "usage: trackd token <add|list|revoke> [flags]"); done {
		return err
	}
	sub, rest := args[0], args[1:]
	fs := flag.NewFlagSet("token "+sub, flag.ContinueOnError)
	db := fs.String("db", defaultDB(), "database path")
	role := fs.String("role", "agent", "token role: agent or admin (add only)")
	if err := fs.Parse(rest); err != nil {
		return err
	}
	switch sub {
	case "add":
		if fs.NArg() != 1 {
			return usagef("usage: trackd token add [--db <path>] [--role agent|admin] <name>")
		}
		st, release, err := openLocked(*db)
		if err != nil {
			return err
		}
		defer st.Close()
		defer release()
		plaintext, err := st.CreateToken(fs.Arg(0), *role)
		if err != nil {
			return err
		}
		fmt.Println(plaintext)
		return nil
	case "list":
		// Read-only: listing tokens while the server runs is safe and common.
		st, err := store.OpenReadOnly(*db)
		if err != nil {
			return openError(*db, err)
		}
		defer st.Close()
		tokens, err := st.ListTokens()
		if err != nil {
			return err
		}
		w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(w, "NAME\tROLE\tCREATED\tLAST USED\tREVOKED")
		for _, t := range tokens {
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", t.Name, t.Role, t.CreatedAt, t.LastUsedAt, t.RevokedAt)
		}
		return w.Flush()
	case "revoke":
		if fs.NArg() != 1 {
			return usagef("usage: trackd token revoke [--db <path>] <name>")
		}
		st, release, err := openLocked(*db)
		if err != nil {
			return err
		}
		defer st.Close()
		defer release()
		if err := st.RevokeToken(fs.Arg(0)); err != nil {
			return err
		}
		fmt.Println("revoked", fs.Arg(0))
		return nil
	default:
		return usagef("unknown token subcommand %q", sub)
	}
}

func cmdBackup(args []string) error {
	fs := flag.NewFlagSet("backup", flag.ContinueOnError)
	db := fs.String("db", defaultDB(), "database path")
	to := fs.String("to", "backups", "snapshot directory")
	keep := fs.Int("keep", 0, "prune all but the newest N snapshots (0 = never prune)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	// Read-only: a snapshot of a database a server is serving is the normal
	// case, and this way the command can never migrate or write to the file.
	s, err := store.OpenReadOnly(*db)
	if err != nil {
		return openError(*db, err)
	}
	defer s.Close()
	snap, err := s.Backup(*to)
	if err != nil {
		return err
	}
	fmt.Println(snap)
	if *keep > 0 {
		removed, err := store.PruneBackups(*to, *keep)
		if err != nil {
			return err
		}
		for _, path := range removed {
			fmt.Println("pruned", path)
		}
	}
	return nil
}

func cmdRestore(args []string) error {
	fs := flag.NewFlagSet("restore", flag.ContinueOnError)
	db := fs.String("db", defaultDB(), "destination database path (must not exist)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return usagef("usage: trackd restore [--db <path>] <snapshot.db>")
	}
	// A live server holds the lock on its own database file. Restore never
	// overwrites an existing file, but check the lock first so the reason
	// given is the real one.
	if _, err := os.Stat(*db); err == nil {
		st, openErr := store.OpenReadOnly(*db)
		if openErr == nil {
			release, lockErr := st.LockExclusive()
			st.Close()
			if lockErr != nil && errors.Is(lockErr, store.ErrLocked) {
				return fmt.Errorf("another trackd is running on %s", *db)
			}
			if release != nil {
				release()
			}
		}
	}
	if err := store.Restore(fs.Arg(0), *db); err != nil {
		return err
	}
	fmt.Println("restored", fs.Arg(0), "to", *db)
	return nil
}

func cmdExport(args []string) error {
	fs := flag.NewFlagSet("export", flag.ContinueOnError)
	db := fs.String("db", defaultDB(), "database path")
	out := fs.String("out", "", "output file (default stdout)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	// Read-only: exporting a database a server is serving is the normal case.
	s, err := store.OpenReadOnly(*db)
	if err != nil {
		return openError(*db, err)
	}
	defer s.Close()
	if *out == "" {
		return s.ExportDump(os.Stdout)
	}
	f, err := os.OpenFile(*out, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	if err := s.ExportDump(f); err != nil {
		f.Close()
		return err
	}
	// A close error here means the dump may be incomplete on disk.
	return f.Close()
}

func cmdImport(args []string) error {
	fs := flag.NewFlagSet("import", flag.ContinueOnError)
	db := fs.String("db", defaultDB(), "database path (must be new or empty)")
	actor := fs.String("actor", "linear-import", "actor recorded on imported issues (linear only)")
	dryRun := fs.Bool("dry-run", false, "parse and validate, report counts, write nothing (linear only)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 2 {
		return usagef("usage: trackd import [--db <path>] <format> <file> (formats: trackd, linear)")
	}
	format, path := fs.Arg(0), fs.Arg(1)
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	s, release, err := openLocked(*db)
	if err != nil {
		return err
	}
	defer s.Close()
	defer release()
	switch format {
	case "trackd":
		if err := s.ImportDump(f); err != nil {
			return err
		}
		fmt.Println("imported", path, "into", *db)
		return nil
	case "linear":
		stats, err := s.ImportLinearCSV(f, *actor, *dryRun)
		if err != nil {
			return err
		}
		verb := "imported"
		if *dryRun {
			verb = "dry run: would import"
		}
		fmt.Printf("%s %d issues, %d projects, %d labels into %s\n", verb, stats.Issues, stats.Projects, stats.Labels, *db)
		for typ, n := range stats.Relations {
			fmt.Printf("  relations (%s): %d\n", typ, n)
		}
		if len(stats.StatusesCreated) > 0 {
			fmt.Printf("  statuses created: %s\n", strings.Join(stats.StatusesCreated, ", "))
		}
		fmt.Printf("  issue keys continue from %s-%d\n", stats.Prefix, stats.Seq+1)
		for _, s := range stats.Skipped {
			fmt.Println("  skipped:", s)
		}
		fmt.Println("note: Linear CSV exports do not include comments; comments are not migrated")
		return nil
	default:
		return usagef("unknown import format %q (formats: trackd, linear)", format)
	}
}

func usage() {
	fmt.Print(`trackd, self-hosted task tracking for AI agents

Usage:
  trackd <command> [flags]

Server commands (operate on the database file directly):
  serve     run the server                             (--db, --addr, --backup-dir, --backup-every, --backup-keep, --backup-timeout)
  token     manage API tokens: add | list | revoke     (--db, --role)
  setting   view or change settings: list | get | set  (--db) [key] [value]
  backup    write a verified snapshot of the database  (--db, --to, --keep)
  restore   restore a snapshot to a new database file  (--db)
  export    dump the full database as JSONL            (--db, --out)
  import    load a dump into a new database            (--db) <format> <file>

Client commands (talk to a running server; --url/--token or $TRACKD_URL/$TRACKD_TOKEN):
  issue     list | show | create | update | append | comment | relate | events
  comment   edit
  events    the global activity feed
  project   list | show | create | update
  milestone list | create | update
  label     list | add
  statuses  list workflow statuses
  health    show server health

  version   print the version
  help      show this help

All client commands accept --json for machine-readable output, on failure too.
Exit codes: 0 ok, 1 unexpected, 2 usage or validation, 3 not found, 4 auth,
5 conflict, 6 server or network.
The database path defaults to $TRACKD_DB, then ./trackd.db.
`)
}

// cmdSetting reads and writes the operator-facing settings: the issue key
// prefix, the exclusive label groups and the base URL. Reads open the file
// read-only; a write takes the exclusive lock like every other server command
// that changes the database, so stop the server first.
func cmdSetting(args []string) error {
	if done, err := groupUsage(args, "usage: trackd setting <list|get|set> [--db <path>] [--json] [key] [value]"); done {
		return err
	}
	sub, rest := args[0], args[1:]
	fs := flag.NewFlagSet("setting "+sub, flag.ContinueOnError)
	db := fs.String("db", defaultDB(), "database path")
	jsonOut := fs.Bool("json", false, "machine-readable output")
	actor := fs.String("actor", "", "actor recorded on the audit trail (set only; default cli)")
	if err := fs.Parse(rest); err != nil {
		return err
	}
	switch sub {
	case "list":
		if fs.NArg() != 0 {
			return usagef("usage: trackd setting list [--db <path>] [--json]")
		}
		st, err := store.OpenReadOnly(*db)
		if err != nil {
			return openError(*db, err)
		}
		defer st.Close()
		list, err := st.ListSettings()
		if err != nil {
			return err
		}
		if *jsonOut {
			return printJSON(map[string]any{"settings": list})
		}
		w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(w, "KEY\tVALUE\tDESCRIPTION")
		for _, v := range list {
			fmt.Fprintf(w, "%s\t%s\t%s\n", v.Key, v.Value, v.Description)
		}
		return w.Flush()
	case "get":
		if fs.NArg() != 1 {
			return usagef("usage: trackd setting get [--db <path>] [--json] <key>")
		}
		key := fs.Arg(0)
		st, err := store.OpenReadOnly(*db)
		if err != nil {
			return openError(*db, err)
		}
		defer st.Close()
		list, err := st.ListSettings()
		if err != nil {
			return err
		}
		for _, v := range list {
			if v.Key == key {
				if *jsonOut {
					return printJSON(v)
				}
				fmt.Println(v.Value)
				return nil
			}
		}
		return usagef("unknown setting %q (run: trackd setting list)", key)
	case "set":
		if fs.NArg() != 2 {
			return usagef("usage: trackd setting set [--db <path>] [--actor <name>] <key> <value>")
		}
		key, value := fs.Arg(0), fs.Arg(1)
		if err := store.ValidateSetting(key, value); err != nil {
			return usagef("%v", err)
		}
		st, release, err := openLocked(*db)
		if err != nil {
			return err
		}
		defer st.Close()
		defer release()
		if err := st.UpdateSetting(key, value, *actor); err != nil {
			return err
		}
		if *jsonOut {
			return printJSON(map[string]string{"key": key, "value": value})
		}
		fmt.Printf("%s = %s\n", key, value)
		return nil
	default:
		return usagef("unknown setting subcommand %q", sub)
	}
}
