package main

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/danmestas/notify/internal/agents/runtime/mesh"
)

// config is the single interpreted view of the process environment for
// `notify serve`. Every flag/env read the server needs is resolved once, here,
// so the services under internal/ receive primitive inputs through their
// constructors instead of reaching into os.Getenv at wiring time. cmd/notify is
// the one place the environment is interpreted.
type config struct {
	// Core transport / storage (flag- and env-overridable).
	natsURL   string
	bridgeURL string
	// bridgeToken authenticates notify against the dagnats worker bridge.
	// Env-only, and deliberately not a flag: a flag value is visible to every
	// user on the box via ps. The name matches the variable dagnats itself
	// reads, so a host that already configured the sidecar needs no new
	// setting.
	bridgeToken string
	// sendmailCommand is the argv the "sendmail" email provider pipes each
	// message to. Host configuration like dbPath: it names a binary on this
	// machine, so it is a flag/env rather than a per-tenant integration field
	// -- otherwise any API-key holder could run an arbitrary command here.
	sendmailCommand []string
	dbPath          string
	addr            string
	dev             bool
	masterKey       []byte
	meshAllow       string
	meshNATSURL     string

	// Comma-separated exact hosts exempted from the outbound SSRF guard.
	// Empty -- the default -- blocks every internal range, as before.
	ssrfAllowedHosts string

	// Inbound email loop.
	inboundSecret string
	inboundMXHost string
	// smtpAddr, when set, starts the embedded SMTP receiver on that address so
	// the inbound MX host can be notify itself. Empty (the default) leaves it
	// off: notify becomes internet-facing on a protocol it was not previously
	// hardened for, so the operator chooses it explicitly.
	smtpAddr string
	// smtpCert and smtpKey name the STARTTLS certificate and key files for
	// the SMTP receiver. Both or neither; with neither it speaks plaintext.
	smtpCert string
	smtpKey  string

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
	natsURL := fs.String("nats-url", envOr("NOTIFY_NATS_URL", "nats://127.0.0.1:4222"), "NATS client URL of the running dagnats sidecar (notify does not embed it)")
	bridgeURL := fs.String("bridge-url", envOr("NOTIFY_BRIDGE_URL", "http://127.0.0.1:8080"), "worker bridge URL of the running dagnats sidecar")
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
		natsURL:     *natsURL,
		bridgeURL:   *bridgeURL,
		bridgeToken: os.Getenv("DAGNATS_BRIDGE_TOKEN"),
		// Whitespace split, no shell: there is no quoting to get wrong and
		// nothing an operator-typed value could inject through.
		sendmailCommand: strings.Fields(*sendmailCommand),
		dbPath:          *dbPath,
		addr:            *addr,
		dev:             *dev,
		masterKey:       mk,
		meshAllow:       *meshAllow,
		meshNATSURL:     *meshNATSURL,

		ssrfAllowedHosts: *ssrfAllow,

		inboundSecret: os.Getenv("NOTIFY_INBOUND_SECRET"),
		smtpAddr:      *smtpAddr,
		smtpCert:      *smtpCert,
		smtpKey:       *smtpKey,
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
