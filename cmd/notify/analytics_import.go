package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"time"

	"github.com/danmestas/notify/internal/analyticslake"
	"github.com/danmestas/notify/internal/store"
	"github.com/danmestas/notify/internal/timestamp"
)

// runAnalyticsImport is `notify analytics-import`: the one-shot copy of the
// SQLite analytics_deliveries rows created before the live NOTIFY_ANALYTICS
// publisher into the analytics lake (#119), as stream notify-import. A rerun
// over the same rows writes nothing new. The R2 credential comes from the
// environment only (NOTIFY_LAKE_R2_ACCESS_KEY_ID, NOTIFY_LAKE_R2_SECRET_ACCESS_KEY)
// and is never printed.
func runAnalyticsImport(args []string, stdout io.Writer, getenv func(string) string) error {
	fs := flag.NewFlagSet("analytics-import", flag.ContinueOnError)
	dbPath := fs.String("db", firstNonEmpty(getenv("NOTIFY_DB"), "notify.db"), "SQLite database path")
	cutoff := fs.String("cutoff", analyticslake.DefaultCutoff.Format(time.RFC3339),
		"import rows created before this RFC3339 instant (when the live publisher started)")
	lakeSel := fs.String("lake", "r2",
		`"r2" for the R2 lake from the NOTIFY_LAKE_R2_* variables, or a local lake root directory`)
	endpoint := fs.String("endpoint", getenv(analyticslake.EnvEndpoint), "R2 endpoint URL (default $"+analyticslake.EnvEndpoint+")")
	bucket := fs.String("bucket", firstNonEmpty(getenv(analyticslake.EnvBucket), analyticslake.DefaultBucket), "R2 bucket")
	prefix := fs.String("prefix", firstNonEmpty(getenv(analyticslake.EnvPrefix), analyticslake.DefaultPrefix), "key prefix in the bucket")
	dryRun := fs.Bool("dry-run", false, "count rows per day and write nothing")
	if err := fs.Parse(args); err != nil {
		return err
	}
	at, err := time.Parse(time.RFC3339Nano, *cutoff)
	if err != nil {
		return fmt.Errorf("-cutoff %q: %w", *cutoff, err)
	}
	flagged := map[string]string{
		analyticslake.EnvEndpoint: *endpoint, analyticslake.EnvBucket: *bucket, analyticslake.EnvPrefix: *prefix,
	}
	// A dry run touches no lake, so it needs no credential.
	var cfg analyticslake.Config
	if !*dryRun {
		cfg, err = analyticslake.ConfigFromEnv(*lakeSel, func(key string) string {
			if v, ok := flagged[key]; ok {
				return v
			}
			return getenv(key)
		})
		if err != nil {
			return err
		}
		if !cfg.Enabled() {
			return fmt.Errorf("-lake is required")
		}
	}

	ctx := context.Background()
	st, err := store.Open(ctx, *dbPath)
	if err != nil {
		return fmt.Errorf("store.Open: %w", err)
	}
	defer func() { _ = st.Close() }()
	plan, err := analyticslake.NewPlan(ctx, st, at)
	if err != nil {
		return err
	}

	_, _ = fmt.Fprintf(stdout, "rows created before %s\n", timestamp.Format(at))
	_, _ = fmt.Fprintln(stdout, "dt\trows")
	for _, day := range plan.Days() {
		_, _ = fmt.Fprintf(stdout, "%s\t%d\n", day.Dt, day.Rows)
	}
	_, _ = fmt.Fprintf(stdout, "total\t%d\n", plan.Rows())
	if *dryRun {
		_, _ = fmt.Fprintln(stdout, "dry run: nothing written")
		return nil
	}

	dst, err := cfg.Store()
	if err != nil {
		return err
	}
	_, _ = fmt.Fprintf(stdout, "writing to %s as stream %s\n", cfg, analyticslake.ImportStream)
	commits, err := plan.Write(ctx, dst)
	for _, c := range commits {
		_, _ = fmt.Fprintf(stdout, "%s\t%d rows\tseq %d-%d\n", c.Key, c.Rows, c.SeqMin, c.SeqMax)
	}
	if err != nil {
		return fmt.Errorf("write: %w", err)
	}
	_, _ = fmt.Fprintf(stdout, "committed %d parts\n", len(commits))
	return nil
}
