package analyticslake

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"path"
	"strings"

	"github.com/danmestas/dagnats/lake"
	"github.com/danmestas/dagnats/lake/s3store"
	"github.com/danmestas/dagnats/lake/writer"
)

// Environment variables for the R2 lake. The credential is notify's own,
// scoped to the lode bucket; it is read from the environment only, never from
// a flag, so it stays out of process listings.
const (
	EnvEndpoint  = "NOTIFY_LAKE_R2_ENDPOINT"
	EnvAccessKey = "NOTIFY_LAKE_R2_ACCESS_KEY_ID"
	EnvSecretKey = "NOTIFY_LAKE_R2_SECRET_ACCESS_KEY"
	EnvBucket    = "NOTIFY_LAKE_BUCKET" // default DefaultBucket
	EnvPrefix    = "NOTIFY_LAKE_PREFIX" // default DefaultPrefix
)

const (
	DefaultBucket = "lode"
	DefaultPrefix = "mesh"
	// r2Region is the SigV4 region R2 expects.
	r2Region = "auto"
)

// Config says where the lake is: a local directory (Dir) or an S3-compatible
// bucket and prefix (R2). The zero value is no lake.
type Config struct {
	Dir string

	Endpoint  string
	Bucket    string
	Prefix    string
	AccessKey string
	SecretKey string
}

// ConfigFromEnv reads a lake selector: "" is no lake, "r2" is the R2 lake from
// the NOTIFY_LAKE_R2_* variables (bucket and prefix default to lode and mesh),
// and anything else is a local lake root directory.
func ConfigFromEnv(selector string, getenv func(string) string) (Config, error) {
	switch selector {
	case "":
		return Config{}, nil
	case "r2":
		cfg := Config{
			Endpoint: getenv(EnvEndpoint), Bucket: orDefault(getenv(EnvBucket), DefaultBucket),
			Prefix:    orDefault(getenv(EnvPrefix), DefaultPrefix),
			AccessKey: getenv(EnvAccessKey), SecretKey: getenv(EnvSecretKey),
		}
		if cfg.Endpoint == "" || cfg.AccessKey == "" || cfg.SecretKey == "" {
			return Config{}, fmt.Errorf("analytics lake r2: %s, %s and %s are required",
				EnvEndpoint, EnvAccessKey, EnvSecretKey)
		}
		return cfg, nil
	default:
		return Config{Dir: selector}, nil
	}
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

// Enabled reports whether a lake is configured.
func (c Config) Enabled() bool { return c.Dir != "" || c.Bucket != "" }

// IsS3 reports whether the lake is a bucket rather than a local directory.
func (c Config) IsS3() bool { return c.Dir == "" && c.Bucket != "" }

// String names the lake without its credentials.
func (c Config) String() string {
	switch {
	case c.Dir != "":
		return "dir " + c.Dir
	case c.Bucket != "":
		return fmt.Sprintf("s3://%s/%s at %s", c.Bucket, c.Prefix, c.Endpoint)
	default:
		return "none"
	}
}

// Store is the lake writer's store for c.
func (c Config) Store() (writer.Store, error) {
	if c.Dir != "" {
		return writer.NewLocalStore(c.Dir), nil
	}
	return c.s3()
}

func (c Config) s3() (*s3store.Store, error) {
	if !c.IsS3() {
		return nil, errors.New("analytics lake: no lake configured")
	}
	return s3store.New(s3store.Config{
		Endpoint: c.Endpoint, Region: r2Region, Bucket: c.Bucket, Prefix: c.Prefix,
		AccessKey: c.AccessKey, SecretKey: c.SecretKey,
	})
}

// EndpointHost is the endpoint's host[:port], for readers (DuckDB's S3
// secret) that take it without a scheme, and whether it is https.
func (c Config) EndpointHost() (host string, https bool, err error) {
	u, err := url.Parse(c.Endpoint)
	if err != nil || u.Host == "" {
		return "", false, fmt.Errorf("analytics lake: endpoint %q: want a URL like https://host", c.Endpoint)
	}
	return u.Host, u.Scheme != "http", nil
}

// LiveParts lists the parts of notify's analytics table a reader may trust:
// those with a commit record and no supersede record. A local lake returns
// file paths; an S3 lake returns s3://bucket/prefix/key URLs.
func (c Config) LiveParts(ctx context.Context) ([]string, error) {
	if c.Dir != "" {
		listing, err := lake.ListCommitted(c.Dir, Program, Table)
		if err != nil {
			return nil, err
		}
		parts := make([]string, 0, len(listing.Commits))
		for _, commit := range listing.Commits {
			parts = append(parts, commit.Path)
		}
		return parts, nil
	}
	st, err := c.s3()
	if err != nil {
		return nil, err
	}
	keys, err := st.List(ctx, Program+"/"+Table+"/", lake.ListedCommitsMax)
	if err != nil {
		return nil, err
	}
	live := liveKeys(keys)
	base := "s3://" + c.Bucket + "/"
	if prefix := strings.Trim(c.Prefix, "/"); prefix != "" {
		base += prefix + "/"
	}
	for i, key := range live {
		live[i] = base + key
	}
	return live, nil
}

// liveKeys keeps the parquet keys whose commit record is present and whose
// supersede record is not, in key order.
func liveKeys(keys []string) []string {
	present := make(map[string]bool, len(keys))
	for _, key := range keys {
		present[key] = true
	}
	var live []string
	for _, key := range keys {
		stem, ok := strings.CutSuffix(key, ".parquet")
		if !ok || !strings.HasPrefix(path.Base(stem), "part-") {
			continue
		}
		if present[stem+".json"] && !present[stem+lake.SupersededSuffix] {
			live = append(live, key)
		}
	}
	return live
}
