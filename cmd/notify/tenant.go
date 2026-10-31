package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/identity"
	"github.com/danmestas/notify/internal/store"
)

// runRotateKey mints a fresh API key for an EXISTING org/environment.
//
// It exists because the raw key is only ever shown once (identity stores a
// hash), and the console's regenerate button is reachable only by an operator
// who is already logged in — which requires the very key they have lost. That
// is a genuine lockout: bootstrap cannot help, because it provisions a NEW
// tenant rather than re-keying an existing one, leaving the real workflows and
// subscribers behind.
//
// Org and environment are resolved by name so an operator does not have to dig
// ULIDs out of SQLite. Rotation revokes every existing key for the scope (see
// identity.RotateKey), so any client still holding the old key breaks.
func runRotateKey(args []string) error {
	fs := flag.NewFlagSet("rotate-key", flag.ExitOnError)
	org := fs.String("org", "", "organization name (or id)")
	env := fs.String("env", "", "environment name (or id)")
	dbPath := fs.String("db", envOr("NOTIFY_DB", "notify.db"), "SQLite database path")
	masterKeyHex := fs.String("master-key-hex", os.Getenv("NOTIFY_MASTER_KEY_HEX"), "32-byte AES master key as hex (required unless -dev)")
	dev := fs.Bool("dev", envBool("NOTIFY_DEV"), "allow rotating without -master-key-hex/NOTIFY_MASTER_KEY_HEX by deriving a fixed, insecure dev key (development only)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *org == "" || *env == "" {
		return fmt.Errorf("--org and --env are required")
	}

	// The master key must match the one serve uses: identity encrypts the
	// stored key material with it, so rotating under a different key mints
	// something serve cannot decrypt.
	mk, err := masterKey(*masterKeyHex, *dev)
	if err != nil {
		return err
	}

	ctx := context.Background()
	st, err := store.Open(ctx, *dbPath)
	if err != nil {
		return fmt.Errorf("store.Open: %w", err)
	}
	defer func() { _ = st.Close() }()

	scope, err := resolveScope(ctx, st, *org, *env)
	if err != nil {
		return err
	}

	key, err := identity.New(st, mk).RotateKey(ctx, scope)
	if err != nil {
		return fmt.Errorf("rotate: %w", err)
	}

	out, _ := json.MarshalIndent(struct {
		APIKey string `json:"apiKey"`
		OrgID  string `json:"orgId"`
		EnvID  string `json:"envId"`
	}{key, scope.OrgID, scope.EnvID}, "", "  ")
	fmt.Println(string(out))
	fmt.Fprintln(os.Stderr, "note: previous keys for this scope are now revoked; the value above is shown once")
	return nil
}

// resolveScope turns human org/env names into a domain.Scope, accepting an id
// for either so the command still works when names are ambiguous. An ambiguous
// name is an error rather than a silent first-match, because rotating the wrong
// environment revokes a key the operator may still be relying on.
func resolveScope(ctx context.Context, st *store.Store, org, env string) (domain.Scope, error) {
	_, reader := st.Pools()

	var orgID string
	rows, err := reader.QueryContext(ctx,
		`SELECT id FROM organizations WHERE id = ? OR name = ?`, org, org)
	if err != nil {
		return domain.Scope{}, fmt.Errorf("lookup org: %w", err)
	}
	orgIDs, err := collectIDs(rows)
	if err != nil {
		return domain.Scope{}, fmt.Errorf("lookup org: %w", err)
	}
	switch len(orgIDs) {
	case 0:
		return domain.Scope{}, fmt.Errorf("no organization named %q", org)
	case 1:
		orgID = orgIDs[0]
	default:
		return domain.Scope{}, fmt.Errorf("organization %q is ambiguous (%d matches); pass its id instead", org, len(orgIDs))
	}

	rows, err = reader.QueryContext(ctx,
		`SELECT id FROM environments WHERE org_id = ? AND (id = ? OR name = ?)`, orgID, env, env)
	if err != nil {
		return domain.Scope{}, fmt.Errorf("lookup env: %w", err)
	}
	envIDs, err := collectIDs(rows)
	if err != nil {
		return domain.Scope{}, fmt.Errorf("lookup env: %w", err)
	}
	switch len(envIDs) {
	case 0:
		return domain.Scope{}, fmt.Errorf("organization %q has no environment named %q", org, env)
	case 1:
		return domain.Scope{OrgID: orgID, EnvID: envIDs[0]}, nil
	default:
		return domain.Scope{}, fmt.Errorf("environment %q is ambiguous (%d matches); pass its id instead", env, len(envIDs))
	}
}

func collectIDs(rows *sql.Rows) ([]string, error) {
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// runBootstrap provisions a tenant (org + env + API key) and prints the key and
// scope so an operator can begin triggering.
func runBootstrap(args []string) error {
	fs := flag.NewFlagSet("bootstrap", flag.ExitOnError)
	org := fs.String("org", "", "organization name")
	env := fs.String("env", "", "environment identifier")
	dbPath := fs.String("db", envOr("NOTIFY_DB", "notify.db"), "SQLite database path")
	masterKeyHex := fs.String("master-key-hex", os.Getenv("NOTIFY_MASTER_KEY_HEX"), "32-byte AES master key as hex (required unless -dev)")
	dev := fs.Bool("dev", envBool("NOTIFY_DEV"), "allow bootstrapping without -master-key-hex/NOTIFY_MASTER_KEY_HEX by deriving a fixed, insecure dev key (development only)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *org == "" || *env == "" {
		return fmt.Errorf("--org and --env are required")
	}

	mk, err := masterKey(*masterKeyHex, *dev)
	if err != nil {
		return err
	}

	ctx := context.Background()
	st, err := store.Open(ctx, *dbPath)
	if err != nil {
		return fmt.Errorf("store.Open: %w", err)
	}
	defer func() { _ = st.Close() }()

	key, scope, err := identity.New(st, mk).Bootstrap(ctx, *org, *env)
	if err != nil {
		return fmt.Errorf("bootstrap: %w", err)
	}

	out, _ := json.MarshalIndent(struct {
		APIKey string `json:"apiKey"`
		OrgID  string `json:"orgId"`
		EnvID  string `json:"envId"`
	}{key, scope.OrgID, scope.EnvID}, "", "  ")
	fmt.Println(string(out))
	return nil
}
