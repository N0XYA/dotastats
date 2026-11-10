package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"text/tabwriter"
	"time"

	"dotastats/internal/collector"
	"dotastats/internal/config"
	"dotastats/internal/db"
	"dotastats/internal/opendota"
)

const accountsUsage = `Usage: dotastats accounts <command> [flags]

Commands:
  list                       show tracked accounts
  add <ID or profile URL>    track an account; the first one becomes the owner
  remove <ID>                stop tracking and delete matches no other account played
  pause <ID>                 keep the data but skip the account when collecting
  resume <ID>                collect a paused account again

Accounts are 32-bit account IDs (Friend ID in Dota 2), SteamID64s, or
OpenDota, Dotabuff, Stratz or Steam profile links.
`

func cmdAccounts(args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		fmt.Fprint(stderr, accountsUsage)
		if len(args) == 0 {
			return errUsage
		}
		return nil
	}
	sub, args := args[0], args[1:]
	operand, n := "<ID>", 1
	switch sub {
	case "list":
		operand, n = "", 0
	case "add":
		operand = "<ID or profile URL>"
	case "remove", "pause", "resume":
	default:
		fmt.Fprintf(stderr, "dotastats accounts: unknown command %q\n\n%s", sub, accountsUsage)
		return errUsage
	}
	fs := newFlagSet("accounts "+sub, operand, stderr)
	dataDirFlag := config.AddDataDirFlag(fs)
	label := new(string)
	if sub == "add" {
		label = fs.String("label", "", "name to show instead of the Steam name")
	}
	operands, err := parseArgs(fs, args, n)
	if err != nil {
		return err
	}

	dataDir, store, err := openStore(*dataDirFlag)
	if err != nil {
		return err
	}
	defer store.Close()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	switch sub {
	case "list":
		accounts, err := collector.ListAccounts(ctx, store.Read)
		if err != nil {
			return err
		}
		printAccounts(stdout, accounts)
		return nil
	case "add":
		col, err := newCollector(ctx, dataDir, store)
		if err != nil {
			return err
		}
		res, err := col.AddAccount(ctx, operands[0], *label)
		if errors.Is(err, collector.ErrNotFound) {
			return fmt.Errorf("%w\n%s", err, collector.ExposeDataHint)
		}
		if err != nil {
			return err
		}
		a := res.Account
		fmt.Fprintf(stdout, "Added %s (%d) as %s. Run \"dotastats backfill\" to fetch its match history.\n",
			a.DisplayName(), a.ID, a.Role)
		if res.NoMatches {
			fmt.Fprintf(stdout, "OpenDota shows no public matches for this account yet.\n%s\n", collector.ExposeDataHint)
		}
		return nil
	}

	id, err := collector.ParseAccountID(operands[0])
	if err != nil {
		return err
	}
	switch sub {
	case "remove":
		deleted, err := collector.RemoveAccount(ctx, store.Write, id)
		if err != nil {
			return err
		}
		fmt.Fprintf(stdout, "Removed %d and %d matches no other tracked account played.\n", id, deleted)
	case "pause", "resume":
		if err := collector.SetAccountActive(ctx, store.Write, id, sub == "resume"); err != nil {
			return err
		}
		fmt.Fprintf(stdout, "%s %d.\n", map[string]string{"pause": "Paused", "resume": "Resumed"}[sub], id)
	}
	return nil
}

func printAccounts(w io.Writer, accounts []collector.Account) {
	if len(accounts) == 0 {
		fmt.Fprintln(w, `No accounts tracked yet. Add one with "dotastats accounts add <ID or profile URL>".`)
		return
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tNAME\tROLE\tSTATUS\tRANK\tMATCHES\tLAST SYNC")
	for _, a := range accounts {
		status, rank, synced := "active", "-", "never"
		if !a.Active {
			status = "paused"
		}
		if a.RankTier != nil {
			rank = strconv.Itoa(*a.RankTier)
		}
		if a.LastSyncedAt != nil {
			synced = a.LastSyncedAt.Local().Format("2006-01-02 15:04")
		}
		fmt.Fprintf(tw, "%d\t%s\t%s\t%s\t%s\t%d\t%s\n", a.ID, a.DisplayName(), a.Role, status, rank, a.Matches, synced)
	}
	tw.Flush()
}

// cmdCollect runs one collection cycle; full (backfill) fetches the whole
// match history of each account instead of only what's new.
func cmdCollect(name string, full bool, args []string, stdout, stderr io.Writer) error {
	fs := newFlagSet(name, "", stderr)
	dataDirFlag := config.AddDataDirFlag(fs)
	account := new(string)
	if full {
		account = fs.String("account", "", "only this account (ID or profile URL)")
	}
	if _, err := parseArgs(fs, args, 0); err != nil {
		return err
	}
	opts := collector.RunOptions{Full: full}
	if *account != "" {
		id, err := collector.ParseAccountID(*account)
		if err != nil {
			return err
		}
		opts.AccountID = id
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

	start := time.Now()
	rep, err := col.Run(ctx, opts)
	printReport(stdout, rep, time.Since(start))
	return err
}

func printReport(w io.Writer, rep collector.Report, took time.Duration) {
	if len(rep.Accounts) == 0 {
		fmt.Fprintln(w, `No active accounts. Add one with "dotastats accounts add <ID or profile URL>".`)
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	for _, r := range rep.Accounts {
		fmt.Fprintf(tw, "%d\t%s\t%d new matches\t%d newly linked\t%d API calls\n",
			r.Account.ID, r.Account.DisplayName(), r.NewMatches, r.NewLinks, r.Calls)
	}
	tw.Flush()
	fmt.Fprintf(w, "Match details stored: %d. Parse requests: %d. Newly parsed: %d.\n", rep.Details, rep.ParseRequests, rep.Parsed)
	fmt.Fprintf(w, "Other API calls (constants, aggregates): %d. Took %s.\n", rep.OtherCalls, took.Round(time.Second))
	if rep.BudgetExhausted {
		fmt.Fprintln(w, "Today's API budget is used up; the next run continues where this one stopped.")
	}
}

// newCollector wires the OpenDota client, daily budget and assets
// directory for a data directory.
func newCollector(ctx context.Context, dataDir string, store *db.DB) (*collector.Collector, error) {
	settings, err := collector.LoadSettings(ctx, store.Read)
	if err != nil {
		return nil, err
	}
	budget := opendota.NewBudget(store.Write, settings.DailyBudget)
	var cacheDir string
	if os.Getenv(config.EnvDevCache) == "1" {
		cacheDir = filepath.Join(dataDir, "dev_cache")
		slog.Info("dev cache on: OpenDota GETs are served from disk when present", "dir", cacheDir)
	}
	api := opendota.New(opendota.Options{
		APIKey:    os.Getenv(config.EnvAPIKey),
		UserAgent: "dotastats/" + version,
		Budget:    budget,
		CacheDir:  cacheDir,
	})
	return collector.New(store, api, budget, collector.Config{AssetsDir: filepath.Join(dataDir, "assets")}), nil
}

// lockDataDir takes the collector lock, explaining what to do when another
// dotastats process holds it.
func lockDataDir(dataDir string) (*collector.Lock, error) {
	lock, err := collector.AcquireLock(dataDir)
	if errors.Is(err, collector.ErrLocked) {
		return nil, fmt.Errorf("%w (%s).\nIf dotastats is running in the background, use its page instead, or stop it and try again", err, dataDir)
	}
	return lock, err
}

func cmdPrune(args []string, stdout, stderr io.Writer) error {
	fs := newFlagSet("prune", "", stderr)
	dataDirFlag := config.AddDataDirFlag(fs)
	if _, err := parseArgs(fs, args, 0); err != nil {
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

	settings, err := collector.LoadSettings(ctx, store.Read)
	if err != nil {
		return err
	}
	sizeBefore := fileSize(config.DBPath(dataDir))
	rep, err := collector.Prune(ctx, store.Write, settings, time.Now())
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "Deleted raw JSON of %d matches, detail of %d, and %d matches.\n",
		rep.RawDeleted, rep.DetailsDeleted, rep.MatchesDeleted)
	fmt.Fprintf(stdout, "Freed %s; database file %s -> %s.\n",
		formatBytes(rep.FreedBytes), formatBytes(sizeBefore), formatBytes(fileSize(config.DBPath(dataDir))))
	return nil
}

func cmdReextract(args []string, stdout, stderr io.Writer) error {
	fs := newFlagSet("reextract", "", stderr)
	dataDirFlag := config.AddDataDirFlag(fs)
	if _, err := parseArgs(fs, args, 0); err != nil {
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

	start := time.Now()
	rep, err := collector.Reextract(ctx, store)
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "Rebuilt %d matches from raw JSON in %s; %d matches have no raw JSON left and were kept as they are.\n",
		rep.Matches, time.Since(start).Round(time.Millisecond), rep.Skipped)
	return nil
}

func fileSize(path string) int64 {
	fi, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return fi.Size()
}

func formatBytes(n int64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d B", n)
}
