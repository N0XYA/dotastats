// Command dotastats is a self-hosted Dota 2 stats service: web server,
// background collector and CLI in one binary. See ROADMAP.md.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"dotastats/internal/collector"
	"dotastats/internal/config"
	"dotastats/internal/db"
	"dotastats/internal/web"
)

// version is set at build time with -ldflags "-X main.version=<tag>".
var version = "dev"

const usage = `Usage: dotastats [command] [flags]

Commands:
  serve      run the web server and background collector (default)
  collect    run one collection cycle
  backfill   fetch full match history for tracked accounts
  accounts   add, remove, pause or list tracked accounts
  prune      apply the retention policy
  reextract  rebuild tables and insights from stored raw JSON
  migrate    apply database migrations
  version    print the version

Run "dotastats <command> -h" for the flags of a command.
`

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run executes one command and returns the process exit code: 0 on
// success, 1 on failure, 2 on bad usage.
func run(args []string, stdout, stderr io.Writer) int {
	// No command, or flags only, means serve: double-clicking the binary
	// must just work.
	cmd := "serve"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		cmd, args = args[0], args[1:]
	}

	var err error
	switch cmd {
	case "serve":
		err = cmdServe(args, stdout, stderr)
	case "migrate":
		err = cmdMigrate(args, stdout, stderr)
	case "accounts":
		err = cmdAccounts(args, stdout, stderr)
	case "collect":
		err = cmdCollect("collect", false, args, stdout, stderr)
	case "backfill":
		err = cmdCollect("backfill", true, args, stdout, stderr)
	case "prune":
		err = cmdPrune(args, stdout, stderr)
	case "reextract":
		err = cmdReextract(args, stdout, stderr)
	case "version":
		fmt.Fprintln(stdout, "dotastats", version)
	case "help":
		fmt.Fprint(stdout, usage)
	default:
		fmt.Fprintf(stderr, "dotastats: unknown command %q\n\n%s", cmd, usage)
		return 2
	}

	switch {
	case err == nil:
		return 0
	case errors.Is(err, flag.ErrHelp):
		return 0
	case errors.Is(err, errUsage):
		return 2
	default:
		fmt.Fprintf(stderr, "dotastats %s: %v\n", cmd, err)
		return 1
	}
}

// errUsage reports a flag error that the FlagSet has already printed.
var errUsage = errors.New("usage error")

// newFlagSet returns a FlagSet for "dotastats <name>"; operands names the
// positional arguments in the usage line.
func newFlagSet(name, operands string, stderr io.Writer) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		fmt.Fprintf(stderr, "Usage: dotastats %s [flags] %s\n\nFlags:\n", name, operands)
		fs.PrintDefaults()
	}
	return fs
}

// parseArgs parses flags anywhere among args and returns the n positional
// arguments, so both "add 123 --label x" and "add --label x 123" work.
func parseArgs(fs *flag.FlagSet, args []string, n int) ([]string, error) {
	var operands []string
	for {
		if err := fs.Parse(args); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				return nil, err
			}
			return nil, errUsage
		}
		if fs.NArg() == 0 {
			break
		}
		operands = append(operands, fs.Arg(0))
		args = fs.Args()[1:]
	}
	if len(operands) != n {
		fmt.Fprintf(fs.Output(), "expected %d argument(s), got %d\n", n, len(operands))
		fs.Usage()
		return nil, errUsage
	}
	return operands, nil
}

func cmdServe(args []string, stdout, stderr io.Writer) error {
	fs := newFlagSet("serve", "", stderr)
	dataDirFlag := config.AddDataDirFlag(fs)
	addr := fs.String("addr", config.DefaultAddr, "listen address; must not be 0.0.0.0")
	noBrowser := fs.Bool("no-browser", false, "don't open the browser on start")
	if _, err := parseArgs(fs, args, 0); err != nil {
		return err
	}
	if err := config.ValidateAddr(*addr); err != nil {
		return err
	}

	dataDir, store, err := openStore(*dataDirFlag)
	if err != nil {
		return err
	}
	defer store.Close()
	lock, err := lockDataDir(dataDir)
	if err != nil {
		return err
	}
	defer lock.Release()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	col, err := newCollector(ctx, dataDir, store)
	if err != nil {
		return err
	}
	sched := collector.NewScheduler(col)
	srv, err := web.New(web.Options{Store: store, Collector: col, Scheduler: sched, Version: version, DataDir: dataDir})
	if err != nil {
		return err
	}

	// On shutdown the scheduler stops at its next step boundary; wait for
	// it before the database closes.
	schedDone := make(chan struct{})
	go func() {
		defer close(schedDone)
		sched.Run(ctx)
	}()
	defer func() { <-schedDone }()
	defer stop()

	return web.Serve(ctx, *addr, srv.Handler(), func(url string) {
		fmt.Fprintf(stdout, "DotaStats is running at %s (press Ctrl+C to stop)\n", url)
		if *noBrowser {
			return
		}
		if err := web.OpenBrowser(url); err != nil {
			slog.Warn("could not open the browser; open the URL manually", "err", err)
		}
	})
}

func cmdMigrate(args []string, stdout, stderr io.Writer) error {
	fs := newFlagSet("migrate", "", stderr)
	dataDirFlag := config.AddDataDirFlag(fs)
	if _, err := parseArgs(fs, args, 0); err != nil {
		return err
	}
	_, store, err := openStore(*dataDirFlag)
	if err != nil {
		return err
	}
	defer store.Close()
	v, err := db.UserVersion(context.Background(), store.Read)
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "schema is at version %d\n", v)
	return nil
}

// openStore resolves and creates the data directory, opens the database
// and brings its schema up to date.
func openStore(dataDirFlag string) (string, *db.DB, error) {
	dataDir, err := config.DataDir(dataDirFlag)
	if err != nil {
		return "", nil, err
	}
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return "", nil, fmt.Errorf("create data directory: %w", err)
	}
	store, err := db.Open(config.DBPath(dataDir))
	if err != nil {
		return "", nil, err
	}
	from, to, err := db.Migrate(context.Background(), store.Write)
	if err != nil {
		store.Close()
		return "", nil, err
	}
	if from != to {
		slog.Info("migrated database", "from", from, "to", to)
	}
	slog.Info("using data directory", "path", dataDir)
	return dataDir, store, nil
}
