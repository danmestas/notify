package main

import (
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"fmt"
	"os"

	"github.com/danmestas/notify/internal/agents/runtime/mesh"
)

// config is the single interpreted view of the process environment for
// `notify serve`. Every flag/env read the server needs is resolved once, here,
// so the services under internal/ receive primitive inputs through their
// constructors instead of reaching into os.Getenv at wiring time. cmd/notify is
// the one place the environment is interpreted.
type config struct {
	// Core transport / storage (flag- and env-overridable).
	natsURL     string
	bridgeURL   string
	dbPath      string
	addr        string
	dev         bool
	masterKey   []byte
	meshAllow   string
	meshNATSURL string

	// Inbound email loop.
	inboundSecret string
	inboundMXHost string

	// Chat two-way loop (Telegram/Discord).
	telegramBotToken  string
	discordBotToken   string
	chatDefaultAgent  string
	discordRespondAll bool
	chatDefaultOrg    string
	chatDefaultEnv    string

	// Console-facing configuration.
	dagnatsConsoleURL string
	aiConsoleKey      string
	consoleReadOnly   bool
	publicBaseURL     string

	// OpenTelemetry tracing. An empty otelEndpoint disables tracing entirely.
	otelEndpoint    string
	otelServiceName string
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
	natsURL := fs.String("nats-url", envOr("NOTIFY_NATS_URL", "nats://127.0.0.1:4222"), "dagnats NATS client URL")
	bridgeURL := fs.String("bridge-url", envOr("NOTIFY_BRIDGE_URL", "http://127.0.0.1:8080"), "dagnats worker bridge URL")
	dbPath := fs.String("db", envOr("NOTIFY_DB", "notify.db"), "SQLite database path")
	addr := fs.String("addr", envOr("NOTIFY_ADDR", "127.0.0.1:3000"), "HTTP listen address")
	masterKeyHex := fs.String("master-key-hex", os.Getenv("NOTIFY_MASTER_KEY_HEX"), "32-byte AES master key as hex (required unless -dev)")
	dev := fs.Bool("dev", envBool("NOTIFY_DEV"), "allow booting without -master-key-hex/NOTIFY_MASTER_KEY_HEX by deriving a fixed, insecure dev key (development only)")
	meshAllow := fs.String("mesh-allow", envOr("NOTIFY_MESH_ALLOW", mesh.DefaultAllowedSubjects), "allowlist pattern for mesh agent dispatch subjects")
	meshNATSURL := fs.String("mesh-nats-url", envOr("NOTIFY_MESH_NATS_URL", ""), "NATS URL for mesh agent dispatch; empty reuses -nats-url (set when agents live on a different hub than the engine)")
	if err := fs.Parse(args); err != nil {
		return config{}, err
	}

	mk, err := masterKey(*masterKeyHex, *dev)
	if err != nil {
		return config{}, err
	}

	return config{
		natsURL:     *natsURL,
		bridgeURL:   *bridgeURL,
		dbPath:      *dbPath,
		addr:        *addr,
		dev:         *dev,
		masterKey:   mk,
		meshAllow:   *meshAllow,
		meshNATSURL: *meshNATSURL,

		inboundSecret: os.Getenv("NOTIFY_INBOUND_SECRET"),
		inboundMXHost: os.Getenv("NOTIFY_INBOUND_MX_HOST"),

		telegramBotToken:  os.Getenv("TELEGRAM_BOT_TOKEN"),
		discordBotToken:   os.Getenv("DISCORD_BOT_TOKEN"),
		chatDefaultAgent:  os.Getenv("NOTIFY_CHAT_DEFAULT_AGENT"),
		discordRespondAll: envBool("NOTIFY_DISCORD_RESPOND_ALL"),
		chatDefaultOrg:    os.Getenv("NOTIFY_CHAT_DEFAULT_ORG"),
		chatDefaultEnv:    os.Getenv("NOTIFY_CHAT_DEFAULT_ENV"),

		dagnatsConsoleURL: os.Getenv("NOTIFY_DAGNATS_CONSOLE_URL"),
		aiConsoleKey:      firstNonEmpty(os.Getenv("NOTIFY_AI_API_KEY"), os.Getenv("ANTHROPIC_API_KEY")),
		consoleReadOnly:   os.Getenv("NOTIFY_CONSOLE_READ_ONLY") == "1",
		publicBaseURL:     os.Getenv("NOTIFY_PUBLIC_BASE_URL"),

		// Standard OTel names, env-only: tracing is an optional integration
		// like the ones above, and an unset endpoint disables it completely.
		otelEndpoint:    os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT"),
		otelServiceName: envOr("OTEL_SERVICE_NAME", "notify"),
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
