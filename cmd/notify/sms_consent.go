package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"

	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/phoneverify"
	"github.com/danmestas/notify/internal/store"
)

// minKeyIDPrefix is the shortest key id prefix -grant-key and -revoke-key
// accept, so a stray character cannot pick a key by accident.
const minKeyIDPrefix = 8

// runSMSConsent switches operator-attested SMS consent (#142) for an existing
// environment and grants or revokes a key's permission to attest, without
// rotating the key.
//
// It is a command run against the database, like bootstrap and rotate-key,
// rather than an HTTP route: notify has no operator credential apart from
// tenant API keys, and a key must not be able to grant itself the permission.
// Whoever can run it can already read and write the database.
func runSMSConsent(args []string) error { return smsConsentTo(os.Stdout, os.Stderr, args) }

func smsConsentTo(stdout, stderr io.Writer, args []string) error {
	fs := flag.NewFlagSet("sms-consent", flag.ContinueOnError)
	fs.SetOutput(stderr)
	org := fs.String("org", "", "organization name (or id)")
	env := fs.String("env", "", "environment name (or id)")
	dbPath := fs.String("db", envOr("NOTIFY_DB", "notify.db"), "SQLite database path")
	enable := fs.Bool("enable", false, "allow operator-attested SMS consent in the environment (needs an https -policy-url, or one already stored)")
	disable := fs.Bool("disable", false, "stop operator-attested SMS consent; existing attestations are suspended, not cleared")
	policyURL := fs.String("policy-url", "", "https URL of the privacy policy the consent rests on")
	grant := fs.String("grant-key", "", "id (or id prefix) of an API key to allow to attest SMS consent")
	revoke := fs.String("revoke-key", "", "id (or id prefix) of an API key to stop attesting SMS consent")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if *org == "" || *env == "" {
		return fmt.Errorf("--org and --env are required")
	}
	if *enable && *disable {
		return fmt.Errorf("--enable and --disable are mutually exclusive")
	}
	if *grant != "" && *revoke != "" {
		return fmt.Errorf("--grant-key and --revoke-key are mutually exclusive")
	}
	if *policyURL != "" && !*enable && !*disable {
		return fmt.Errorf("--policy-url needs --enable or --disable")
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
	logger := slog.New(slog.NewTextHandler(stderr, nil))

	// Resolve the key before changing anything, so a bad id leaves the
	// environment setting as it was.
	keyRef, allowed := *grant, true
	if *revoke != "" {
		keyRef, allowed = *revoke, false
	}
	var keyID string
	if keyRef != "" {
		if keyID, err = resolveKeyID(ctx, st, scope, keyRef); err != nil {
			return err
		}
	}

	if *enable || *disable {
		if err := setConsentSetting(ctx, st, logger, scope, *enable, *policyURL); err != nil {
			return err
		}
	}
	if keyID != "" {
		if err := st.ApiKeys().SetKeyAttestSMSConsent(ctx, scope, keyID, allowed); err != nil {
			return fmt.Errorf("set attest permission: %w", err)
		}
		logger.Info("sms consent attest permission set",
			"org", scope.OrgID, "env", scope.EnvID, "key", keyID, "allowed", allowed)
	}
	return printConsentState(ctx, stdout, st, scope)
}

// setConsentSetting applies the environment setting with the console's own
// validation (phoneverify.SetConsentSettings). An omitted policyURL keeps the
// stored one.
func setConsentSetting(ctx context.Context, st *store.Store, logger *slog.Logger, scope domain.Scope, enabled bool, policyURL string) error {
	pv := phoneverify.New(st, nil)
	if strings.TrimSpace(policyURL) == "" {
		cur, err := pv.ConsentSettings(ctx, scope)
		if err != nil {
			return fmt.Errorf("read sms consent setting: %w", err)
		}
		policyURL = cur.PolicyURL
	}
	if err := pv.SetConsentSettings(ctx, scope, enabled, policyURL); err != nil {
		if errors.Is(err, phoneverify.ErrPolicyURLRequired) {
			return fmt.Errorf("--policy-url: %w", err)
		}
		return fmt.Errorf("set sms consent setting: %w", err)
	}
	logger.Info("sms consent attestation set",
		"org", scope.OrgID, "env", scope.EnvID, "enabled", enabled, "policy_url", strings.TrimSpace(policyURL))
	return nil
}

// resolveKeyID returns the id (hash) of the one API key in scope whose id
// starts with ref. The plaintext key is never accepted: it does not match an
// id, and an operator should not need it to change the key's permission.
func resolveKeyID(ctx context.Context, st *store.Store, scope domain.Scope, ref string) (string, error) {
	ref = strings.ToLower(strings.TrimSpace(ref))
	if len(ref) < minKeyIDPrefix {
		return "", fmt.Errorf("key id %q is too short; give at least %d characters of it", ref, minKeyIDPrefix)
	}
	keys, err := st.ApiKeys().List(ctx, scope)
	if err != nil {
		return "", fmt.Errorf("list api keys: %w", err)
	}
	var match []string
	for _, k := range keys {
		if strings.HasPrefix(k.Hash, ref) {
			match = append(match, k.Hash)
		}
	}
	switch len(match) {
	case 0:
		return "", fmt.Errorf("environment has no API key with id %q", ref)
	case 1:
		return match[0], nil
	default:
		return "", fmt.Errorf("key id %q is ambiguous (%d matches); give more of it", ref, len(match))
	}
}

// printConsentState writes the environment's setting and its keys' ids and
// permissions as JSON.
func printConsentState(ctx context.Context, w io.Writer, st *store.Store, scope domain.Scope) error {
	settings, err := st.SMSConsents().Settings(ctx, scope)
	if err != nil {
		return fmt.Errorf("read sms consent setting: %w", err)
	}
	keys, err := st.ApiKeys().List(ctx, scope)
	if err != nil {
		return fmt.Errorf("list api keys: %w", err)
	}
	type keyState struct {
		ID               string `json:"id"`
		AttestSMSConsent bool   `json:"attestSmsConsent"`
	}
	out := struct {
		OrgID     string     `json:"orgId"`
		EnvID     string     `json:"envId"`
		Enabled   bool       `json:"enabled"`
		PolicyURL string     `json:"policyUrl"`
		Keys      []keyState `json:"keys"`
	}{scope.OrgID, scope.EnvID, settings.Enabled, settings.PolicyURL, []keyState{}}
	for _, k := range keys {
		out.Keys = append(out.Keys, keyState{k.Hash, k.AttestSMSConsent})
	}
	b, _ := json.MarshalIndent(out, "", "  ")
	_, err = fmt.Fprintln(w, string(b))
	return err
}
