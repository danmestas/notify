package main

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/danmestas/notify/internal/agents/runtime/mesh"
	"github.com/danmestas/notify/internal/aibuilder"
	"github.com/danmestas/notify/internal/blobstore"
	"github.com/danmestas/notify/notify"
)

// config is the single interpreted view of the process environment for
// `notify serve`. Every flag/env read the server needs is resolved once, here,
// so the services under internal/ receive primitive inputs through their
// constructors instead of reaching into os.Getenv at wiring time. cmd/notify is
// the one place the environment is interpreted.
//
// app is what the notify package needs; the rest is the process's own: where
// to dial, where to listen, and the process-wide SSRF and tracing setup.
type config struct {
	natsURL string
	// meshNATSURL is the hub mesh agents are dispatched over; empty reuses
	// natsURL's connection.
	meshNATSURL string
	addr        string

	// Comma-separated exact hosts exempted from the outbound SSRF guard.
	// Empty -- the default -- blocks every internal range, as before.
	ssrfAllowedHosts string

	// OpenTelemetry tracing. An empty otelEndpoint disables tracing entirely.
	otelEndpoint    string
	otelServiceName string

	app notify.Config
}

// envOr returns the environment variable value if set, otherwise def.
func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// envBool reports whether the environment variable key is set to "1" or
// "true" (matches the NOTIFY_DISCORD_RESPOND_ALL convention).
func envBool(key string) bool {
	switch os.Getenv(key) {
	case "1", "true":
		return true
	default:
		return false
	}
}

// masterKey resolves the 32-byte AES-256-GCM master key. A non-empty hex flag
// is decoded and length-checked. An empty flag is only accepted when dev is
// true: it then derives the exact same fixed dev key as before (byte-for-byte
// unchanged) so a local operator can keep booting without ceremony and
// existing local users are not re-keyed. Outside dev, an empty hex key is a
// fatal misconfiguration — silently deriving a fixed, publicly-computable key
// would encrypt every credential and cookie under a value anyone can compute.
func masterKey(hexKey string, dev bool) ([]byte, error) {
	if hexKey == "" {
		if !dev {
			return nil, fmt.Errorf("master-key-hex is required outside -dev/NOTIFY_DEV=1 (an empty key would derive a fixed, publicly-known encryption key)")
		}
		k := sha256.Sum256([]byte("notify-dev-master-key"))
		return k[:], nil
	}
	raw, err := hex.DecodeString(hexKey)
	if err != nil {
		return nil, fmt.Errorf("decode master-key-hex: %w", err)
	}
	if len(raw) != 32 {
		return nil, fmt.Errorf("master-key-hex must decode to 32 bytes, got %d", len(raw))
	}
	return raw, nil
}

// loadConfig parses the `serve` flag set over args, layering flag > env >
// default via envOr (the precedence every scattered read used to enforce
// individually), then resolves the master key. It returns an error rather than
// exiting on the required-key check so the interpretation is testable and the
// single failure point is visible.
func loadConfig(args []string) (config, error) {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	natsURL := fs.String("nats-url", envOr("NOTIFY_NATS_URL", "nats://127.0.0.1:4222"), "NATS client URL of the running dagnats sidecar (notify does not embed it)")
	sendmailCommand := fs.String("sendmail-command", envOr("NOTIFY_SENDMAIL_COMMAND", "/usr/sbin/sendmail -ti"), "command the \"sendmail\" email provider pipes messages to (split on whitespace, run without a shell)")
	smtpAddr := fs.String("smtp", envOr("NOTIFY_SMTP_ADDR", ""), "address for the embedded SMTP receiver, e.g. 0.0.0.0:25 (empty = off); makes the inbound MX host notify itself")
	smtpCert := fs.String("smtp-cert", envOr("NOTIFY_SMTP_CERT", ""), "PEM certificate file for the SMTP receiver's STARTTLS (with -smtp-key); reloaded when it changes")
	smtpKey := fs.String("smtp-key", envOr("NOTIFY_SMTP_KEY", ""), "PEM private key file for the SMTP receiver's STARTTLS (with -smtp-cert)")
	dbPath := fs.String("db", envOr("NOTIFY_DB", "notify.db"), "SQLite database path")
	addr := fs.String("addr", envOr("NOTIFY_ADDR", "127.0.0.1:3000"), "HTTP listen address")
	masterKeyHex := fs.String("master-key-hex", os.Getenv("NOTIFY_MASTER_KEY_HEX"), "32-byte AES master key as hex (required unless -dev)")
	dev := fs.Bool("dev", envBool("NOTIFY_DEV"), "allow booting without -master-key-hex/NOTIFY_MASTER_KEY_HEX by deriving a fixed, insecure dev key (development only)")
	meshAllow := fs.String("mesh-allow", envOr("NOTIFY_MESH_ALLOW", mesh.DefaultAllowedSubjects), "allowlist pattern for mesh agent dispatch subjects")
	meshNATSURL := fs.String("mesh-nats-url", envOr("NOTIFY_MESH_NATS_URL", ""), "NATS URL for mesh agent dispatch; empty reuses -nats-url (set when agents live on a different hub than the engine)")
	ssrfAllow := fs.String("ssrf-allow", envOr("NOTIFY_SSRF_ALLOWED_HOSTS", ""), "comma-separated exact hosts (no wildcards) that outbound HTTP steps may reach despite resolving to an internal address; empty blocks all internal ranges")
	if err := fs.Parse(args); err != nil {
		return config{}, err
	}

	mk, err := masterKey(*masterKeyHex, *dev)
	if err != nil {
		return config{}, err
	}
	if (*smtpCert == "") != (*smtpKey == "") {
		return config{}, errors.New("-smtp-cert and -smtp-key must be set together")
	}

	return config{
		natsURL:          *natsURL,
		meshNATSURL:      *meshNATSURL,
		addr:             *addr,
		ssrfAllowedHosts: *ssrfAllow,
		// Standard OTel names, env-only: tracing is an optional integration,
		// and an unset endpoint disables it completely.
		otelEndpoint:    os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT"),
		otelServiceName: envOr("OTEL_SERVICE_NAME", "notify"),

		app: notify.Config{
			DBPath:    *dbPath,
			MasterKey: mk,
			Dev:       *dev,
			// Whitespace split, no shell: there is no quoting to get wrong and
			// nothing an operator-typed value could inject through.
			SendmailCommand: strings.Fields(*sendmailCommand),
			MeshAllow:       *meshAllow,

			BlobDriver: os.Getenv("NOTIFY_BLOB_DRIVER"),
			BlobDir:    envOr("NOTIFY_BLOB_DIR", filepath.Join(filepath.Dir(*dbPath), "blobs")),
			BlobS3: blobstore.S3Config{
				Endpoint:  os.Getenv("NOTIFY_BLOB_S3_ENDPOINT"),
				Region:    os.Getenv("NOTIFY_BLOB_S3_REGION"),
				Bucket:    os.Getenv("NOTIFY_BLOB_S3_BUCKET"),
				Prefix:    os.Getenv("NOTIFY_BLOB_S3_PREFIX"),
				AccessKey: os.Getenv("NOTIFY_BLOB_S3_ACCESS_KEY"),
				SecretKey: os.Getenv("NOTIFY_BLOB_S3_SECRET_KEY"),
				UseTLS:    envBool("NOTIFY_BLOB_S3_TLS"),
			},

			InboundSecret: os.Getenv("NOTIFY_INBOUND_SECRET"),
			InboundMXHost: os.Getenv("NOTIFY_INBOUND_MX_HOST"),
			SMTPAddr:      *smtpAddr,
			SMTPCert:      *smtpCert,
			SMTPKey:       *smtpKey,

			TelegramBotToken:  os.Getenv("TELEGRAM_BOT_TOKEN"),
			DiscordBotToken:   os.Getenv("DISCORD_BOT_TOKEN"),
			ChatDefaultAgent:  os.Getenv("NOTIFY_CHAT_DEFAULT_AGENT"),
			DiscordRespondAll: envBool("NOTIFY_DISCORD_RESPOND_ALL"),
			ChatDefaultOrg:    os.Getenv("NOTIFY_CHAT_DEFAULT_ORG"),
			ChatDefaultEnv:    os.Getenv("NOTIFY_CHAT_DEFAULT_ENV"),

			DagnatsConsoleURL: os.Getenv("NOTIFY_DAGNATS_CONSOLE_URL"),
			AIConsoleKey:      firstNonEmpty(os.Getenv("NOTIFY_AI_API_KEY"), os.Getenv("ANTHROPIC_API_KEY")),
			ConsoleReadOnly:   os.Getenv("NOTIFY_CONSOLE_READ_ONLY") == "1",
			PublicBaseURL:     os.Getenv("NOTIFY_PUBLIC_BASE_URL"),
			HTTPAddr:          *addr,

			// Provider selection (in priority order):
			//   NOTIFY_AI_API_KEY → OpenRouter (default) or any OpenAI-compatible endpoint
			//     NOTIFY_AI_BASE_URL overrides the base URL (default: https://openrouter.ai/api/v1)
			//     NOTIFY_AI_MODEL   overrides the model     (default: openai/gpt-4.1)
			//   ANTHROPIC_API_KEY  → Anthropic directly (fallback)
			//   Neither            → builder gracefully disabled (Guided tab shows a note)
			AI: aibuilder.NewClientFromEnv(os.Getenv),

			DuckDBPath: envOr("NOTIFY_DUCKDB", "notify-analytics.duckdb"),
		},
	}, nil
}

// firstNonEmpty returns the first non-empty string in vals, or "" if all empty.
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
