package main

import (
	"flag"
	"fmt"
	"os"

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
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 2 {
		return fmt.Errorf("usage: trackd import [--db <path>] <format> <file> (formats: trackd)")
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
	case "linear":
		return fmt.Errorf("linear import is not implemented yet")
	default:
		return fmt.Errorf("unknown import format %q (formats: trackd)", format)
	}
	fmt.Println("imported", path, "into", *db)
	return nil
}

func usage() {
	fmt.Print(`trackd — self-hosted task tracking for AI agents

Usage:
  trackd <command> [flags]

Commands:
  backup    write a verified snapshot of the database  (--db, --to, --keep)
  restore   restore a snapshot to a new database file  (--db)
  export    dump the full database as JSONL            (--db, --out)
  import    load a dump into a new database            (--db) <format> <file>
  version   print the version
  help      show this help

The database path defaults to $TRACKD_DB, then ./trackd.db.
`)
}
