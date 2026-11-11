package main

// Methodology: runBootstrap is driven with real flags against a temp SQLite
// file and its stdout captured, so the printed key is proven by parsing it and
// the repeat-run behaviour by what a second invocation prints and returns.

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
)

func bootstrapArgs(db string) []string {
	return []string{"-org", "Acme", "-env", "production", "-db", db, "-dev"}
}

func TestBootstrapPrintsKeyOnce(t *testing.T) {
	db := filepath.Join(t.TempDir(), "n.db")

	var first bytes.Buffer
	if err := bootstrapTo(&first, bootstrapArgs(db)); err != nil {
		t.Fatalf("first bootstrap: %v", err)
	}
	var got struct{ APIKey, OrgID, EnvID string }
	if err := json.Unmarshal(first.Bytes(), &got); err != nil {
		t.Fatalf("first output is not the key JSON: %q: %v", first.String(), err)
	}
	if got.APIKey == "" || got.OrgID == "" || got.EnvID == "" {
		t.Fatalf("first output = %+v, want apiKey, orgId, envId", got)
	}

	var second bytes.Buffer
	if err := bootstrapTo(&second, bootstrapArgs(db)); err != nil {
		t.Fatalf("second bootstrap must exit 0, got: %v", err)
	}
	out := second.String()
	if strings.Contains(out, got.APIKey) {
		t.Fatalf("second output leaks the key: %q", out)
	}
	for _, want := range []string{"already exists", "rotate-key", got.OrgID, got.EnvID} {
		if !strings.Contains(out, want) {
			t.Errorf("second output %q lacks %q", out, want)
		}
	}
}
