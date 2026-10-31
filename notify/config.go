package notify

import (
	"log/slog"

	"github.com/nats-io/nats.go"

	"github.com/danmestas/notify/internal/aibuilder"
	"github.com/danmestas/notify/internal/blobstore"
)

// Host is what the process running notify provides. Everything in it belongs
// to the host: notify uses the connections but never closes them.
type Host struct {
	// NC is notify's connection to dagnats. It carries the engine's control
	// plane and task worker, and the event bus.
	NC *nats.Conn
	// Mesh is the connection mesh agents are dispatched over. Nil means NC.
	Mesh *nats.Conn
	// Logger receives notify's logs. Nil means slog.Default().
	Logger *slog.Logger
	// BasePath is the path prefix Handler serves every route under, such as
	// "/notify". Empty when notify owns the root of its listener.
	BasePath string
}

// Config is notify's interpreted configuration. The host resolves it from
// flags, the environment or anywhere else; notify reads nothing on its own.
type Config struct {
	// DBPath is the SQLite database notify opens.
	DBPath string
	// MasterKey is the 32-byte AES-256-GCM key that seals credentials, API
	// keys and secret step fields at rest.
	MasterKey []byte
	// Dev relaxes development-only guards: managed agents and agent configs
	// may reach MCP servers on loopback over plain http.
	Dev bool

	// SendmailCommand is the argv the "sendmail" email provider pipes each
	// message to. It names a binary on this machine, so it is host
	// configuration rather than a per-tenant integration field.
	SendmailCommand []string
	// MeshAllow is the allowlist pattern for mesh agent dispatch subjects.
	MeshAllow string

	// Raw inbound mail storage. BlobDriver is "" (off), "fs" or "s3".
	BlobDriver string
	BlobDir    string
	BlobS3     blobstore.S3Config

	// InboundSecret authenticates the mail forwarder's POST to
	// /v1/inbound/email; empty disables inbound email.
	InboundSecret string
	// InboundMXHost is the MX host subscribers reply to.
	InboundMXHost string
	// SMTPAddr, when set, starts the embedded SMTP receiver on that address.
	// SMTPCert and SMTPKey name its STARTTLS files: both or neither.
	SMTPAddr string
	SMTPCert string
	SMTPKey  string

	// Chat two-way loop. An empty bot token disables that platform's poller;
	// an empty default agent drops cold inbound.
	TelegramBotToken  string
	DiscordBotToken   string
	ChatDefaultAgent  string
	DiscordRespondAll bool
	ChatDefaultOrg    string
	ChatDefaultEnv    string

	// Console-facing configuration.
	DagnatsConsoleURL string
	// AIConsoleKey is non-empty when an AI provider is configured; the
	// console only uses it to decide what to display.
	AIConsoleKey    string
	ConsoleReadOnly bool
	PublicBaseURL   string
	// HTTPAddr is the address the host serves Handler on. The console shows
	// it as the API URL; notify does not listen on it.
	HTTPAddr string

	// AI is the durable workflow builder's model client. Nil disables it.
	AI aibuilder.Client

	// DuckDBPath is the analytics database in a `-tags duckdb` build; other
	// builds ignore it.
	DuckDBPath string
}
