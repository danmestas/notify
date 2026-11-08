package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/danmestas/dagnats/lake"

	"github.com/danmestas/notify/internal/analyticslake"
	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/store"
)

func seedAnalytics(t *testing.T) string {
	t.Helper()
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "notify.db")
	st, err := store.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	scope := domain.Scope{OrgID: "o1", EnvID: "e1"}
	for i, d := range []domain.AnalyticsDelivery{
		{Channel: "email", Status: "sent", CreatedAt: "2026-11-01T09:00:00.000000000Z", EventKey: "m1:sent"},
		{Channel: "email", Status: "delivered", CreatedAt: "2026-11-01T09:00:00.000000000Z", EventKey: "m1:delivered"},
		{Channel: "sms", Status: "sent", CreatedAt: "2026-11-03T09:00:00.000000000Z"},
		{Channel: "sms", Status: "sent", CreatedAt: "2026-11-09T09:00:00.000000000Z", EventKey: "m9:sent"},
	} {
		if err := st.Analytics().Insert(ctx, scope, d); err != nil {
			t.Fatalf("insert %d: %v", i, err)
		}
	}
	if err := st.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	return dbPath
}

func noEnv(string) string { return "" }

func TestAnalyticsImport_DryRunWritesNothing(t *testing.T) {
	dbPath := seedAnalytics(t)
	lakeDir := t.TempDir()
	var out bytes.Buffer
	err := runAnalyticsImport([]string{"-db", dbPath, "-lake", lakeDir, "-dry-run"}, &out, noEnv)
	if err != nil {
		t.Fatalf("dry run: %v", err)
	}
	for _, want := range []string{"2026-11-01\t2", "2026-11-03\t1", "total\t3"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("output lacks %q:\n%s", want, out.String())
		}
	}
	entries, err := os.ReadDir(lakeDir)
	if err != nil || len(entries) != 0 {
		t.Fatalf("dry run wrote to the lake: %v %v", entries, err)
	}
}

func TestAnalyticsImport_WritesLocalLakeIdempotently(t *testing.T) {
	dbPath := seedAnalytics(t)
	lakeDir := t.TempDir()
	run := func() string {
		var out bytes.Buffer
		if err := runAnalyticsImport([]string{"-db", dbPath, "-lake", lakeDir}, &out, noEnv); err != nil {
			t.Fatalf("import: %v", err)
		}
		return out.String()
	}
	first := run()
	listing, err := lake.ListCommitted(lakeDir, analyticslake.Program, analyticslake.Table)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	rows := 0
	for _, c := range listing.Commits {
		rows += c.Rows
	}
	if len(listing.Commits) != 2 || rows != 3 {
		t.Fatalf("commits: %d holding %d rows, want 2 holding 3\n%s", len(listing.Commits), rows, first)
	}
	if second := run(); second != first {
		t.Fatalf("rerun reported different parts:\n%s\nvs\n%s", first, second)
	}
	again, err := lake.ListCommitted(lakeDir, analyticslake.Program, analyticslake.Table)
	if err != nil || len(again.Commits) != 2 {
		t.Fatalf("rerun listing: %d commits, %v", len(again.Commits), err)
	}
}

func TestAnalyticsImport_R2NeedsCredentials(t *testing.T) {
	dbPath := seedAnalytics(t)
	err := runAnalyticsImport([]string{"-db", dbPath}, &bytes.Buffer{}, noEnv)
	if err == nil || !strings.Contains(err.Error(), "NOTIFY_LAKE_R2_ACCESS_KEY_ID") {
		t.Fatalf("want a missing-credential error, got %v", err)
	}
}

// TestAnalyticsImport_CutoffFlag also runs a dry run against the default r2
// lake with no credential set: a dry run needs none.
func TestAnalyticsImport_CutoffFlag(t *testing.T) {
	dbPath := seedAnalytics(t)
	var out bytes.Buffer
	err := runAnalyticsImport([]string{
		"-db", dbPath, "-dry-run",
		"-cutoff", "2026-11-02T00:00:00Z",
	}, &out, noEnv)
	if err != nil {
		t.Fatalf("dry run: %v", err)
	}
	if !strings.Contains(out.String(), "total\t2") || strings.Contains(out.String(), "2026-11-03") {
		t.Fatalf("cutoff not applied:\n%s", out.String())
	}
}
