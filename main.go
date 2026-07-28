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
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "trackd:", err)
		os.Exit(1)
	}
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
	case "backup":
		return cmdBackup(rest)
	case "restore":
		return cmdRestore(rest)
	case "export":
		return cmdExport(rest)
	case "import":
		return cmdImport(rest)
	case "help", "-h", "--help":
		usage()
		return nil
	default:
		usage()
		return fmt.Errorf("unknown command %q", cmd)
	}
}

func defaultDB() string {
	if v := os.Getenv("TRACKD_DB"); v != "" {
		return v
	}
	return "trackd.db"
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
	st, err := store.Open(*db)
	if err != nil {
		return err
	}
	defer st.Close()

	tokens, err := st.ListTokens()
	if err != nil {
		return err
	}
	if len(tokens) == 0 {
		plaintext, err := st.CreateToken("admin", "admin")
		if err != nil {
			return err
		}
		fmt.Printf("created initial admin token (store it now; it is never shown again):\n  %s\n", plaintext)
	}

	srv := server.New(st, version)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go srv.RunBackups(ctx, server.BackupConfig{Dir: *backupDir, Every: *backupEvery, Keep: *backupKeep, Timeout: *backupTimeout})

	httpSrv := &http.Server{Addr: *addr, Handler: srv.Handler(), ReadHeaderTimeout: 10 * time.Second}
	shutdownDone := make(chan struct{})
	go func() {
		defer close(shutdownDone)
		<-ctx.Done()
		log.Print("shutting down")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
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
	if len(args) == 0 {
		return fmt.Errorf("usage: trackd token <add|list|revoke> [flags]")
	}
	sub, rest := args[0], args[1:]
	fs := flag.NewFlagSet("token "+sub, flag.ContinueOnError)
	db := fs.String("db", defaultDB(), "database path")
	role := fs.String("role", "agent", "token role: agent or admin (add only)")
	if err := fs.Parse(rest); err != nil {
		return err
	}
	st, err := store.Open(*db)
	if err != nil {
		return err
	}
	defer st.Close()
	switch sub {
	case "add":
		if fs.NArg() != 1 {
			return fmt.Errorf("usage: trackd token add [--db <path>] [--role agent|admin] <name>")
		}
		plaintext, err := st.CreateToken(fs.Arg(0), *role)
		if err != nil {
			return err
		}
		fmt.Println(plaintext)
		return nil
	case "list":
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
			return fmt.Errorf("usage: trackd token revoke [--db <path>] <name>")
		}
		if err := st.RevokeToken(fs.Arg(0)); err != nil {
			return err
		}
		fmt.Println("revoked", fs.Arg(0))
		return nil
	default:
		return fmt.Errorf("unknown token subcommand %q", sub)
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
	s, err := store.Open(*db)
	if err != nil {
		return err
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
		return fmt.Errorf("usage: trackd restore [--db <path>] <snapshot.db>")
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
	s, err := store.Open(*db)
	if err != nil {
		return err
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
		return fmt.Errorf("usage: trackd import [--db <path>] <format> <file> (formats: trackd, linear)")
	}
	format, path := fs.Arg(0), fs.Arg(1)
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	s, err := store.Open(*db)
	if err != nil {
		return err
	}
	defer s.Close()
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
		return fmt.Errorf("unknown import format %q (formats: trackd, linear)", format)
	}
}

func usage() {
	fmt.Print(`trackd — self-hosted task tracking for AI agents

Usage:
  trackd <command> [flags]

Server commands (operate on the database file directly):
  serve     run the server                             (--db, --addr, --backup-dir, --backup-every, --backup-keep, --backup-timeout)
  token     manage API tokens: add | list | revoke     (--db, --role)
  backup    write a verified snapshot of the database  (--db, --to, --keep)
  restore   restore a snapshot to a new database file  (--db)
  export    dump the full database as JSONL            (--db, --out)
  import    load a dump into a new database            (--db) <format> <file>

Client commands (talk to a running server; --url/--token or $TRACKD_URL/$TRACKD_TOKEN):
  issue     list | show | create | update | comment | relate | events
  project   list | show | create | update
  milestone list | create | update
  label     list | add
  statuses  list workflow statuses
  health    show server health

  version   print the version
  help      show this help

All client commands accept --json for machine-readable output.
The database path defaults to $TRACKD_DB, then ./trackd.db.
`)
}
