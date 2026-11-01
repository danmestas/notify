package main

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"
)

// TestLoadConfig_EnvAndFlagPrecedence pins the precedence the scattered envOr
// calls used to enforce individually: an explicit flag beats the environment
// variable, which beats the compiled-in default. loadConfig is the single place
// that ordering now lives, so it is the single place the invariant is proven.
func TestLoadConfig_EnvAndFlagPrecedence(t *testing.T) {
	// Master key present so loadConfig does not fail on the required-key check.
	key := hex.EncodeToString(make([]byte, 32))
	t.Setenv("NOTIFY_MASTER_KEY_HEX", key)

	t.Run("default when neither flag nor env set", func(t *testing.T) {
		t.Setenv("NOTIFY_NATS_URL", "")
		cfg, err := loadConfig(nil)
		if err != nil {
			t.Fatalf("loadConfig: %v", err)
		}
		// Empty: the deprecated flag then defers to program.Run's default.
		if cfg.natsURL != "" {
			t.Fatalf("natsURL default = %q, want empty", cfg.natsURL)
		}
	})

	t.Run("env beats default", func(t *testing.T) {
		t.Setenv("NOTIFY_NATS_URL", "nats://env:4222")
		cfg, err := loadConfig(nil)
		if err != nil {
			t.Fatalf("loadConfig: %v", err)
		}
		if cfg.natsURL != "nats://env:4222" {
			t.Fatalf("natsURL from env = %q, want nats://env:4222", cfg.natsURL)
		}
	})

	t.Run("flag beats env", func(t *testing.T) {
		t.Setenv("NOTIFY_NATS_URL", "nats://env:4222")
		cfg, err := loadConfig([]string{"-nats-url", "nats://flag:4222"})
		if err != nil {
			t.Fatalf("loadConfig: %v", err)
		}
		if cfg.natsURL != "nats://flag:4222" {
			t.Fatalf("natsURL from flag = %q, want nats://flag:4222", cfg.natsURL)
		}
	})
}

// TestLoadConfig_MasterKeyRequiredUnlessDev mirrors the masterKey() contract
// (main.go): an empty hex key is fatal outside dev, but in dev it derives the
// fixed dev key so a local operator can boot without ceremony.
func TestLoadConfig_MasterKeyRequiredUnlessDev(t *testing.T) {
	t.Run("empty key outside dev is fatal", func(t *testing.T) {
		t.Setenv("NOTIFY_MASTER_KEY_HEX", "")
		t.Setenv("NOTIFY_DEV", "")
		if _, err := loadConfig(nil); err == nil {
			t.Fatal("loadConfig with empty key and no dev = nil error, want error")
		}
	})

	t.Run("empty key with -dev derives the fixed dev key", func(t *testing.T) {
		t.Setenv("NOTIFY_MASTER_KEY_HEX", "")
		t.Setenv("NOTIFY_DEV", "")
		cfg, err := loadConfig([]string{"-dev"})
		if err != nil {
			t.Fatalf("loadConfig -dev: %v", err)
		}
		want := sha256.Sum256([]byte("notify-dev-master-key"))
		if string(cfg.app.MasterKey) != string(want[:]) {
			t.Fatal("dev master key mismatch: loadConfig did not derive the fixed dev key")
		}
	})

	t.Run("valid hex key is decoded", func(t *testing.T) {
		raw := make([]byte, 32)
		for i := range raw {
			raw[i] = byte(i)
		}
		t.Setenv("NOTIFY_MASTER_KEY_HEX", hex.EncodeToString(raw))
		t.Setenv("NOTIFY_DEV", "")
		cfg, err := loadConfig(nil)
		if err != nil {
			t.Fatalf("loadConfig: %v", err)
		}
		if string(cfg.app.MasterKey) != string(raw) {
			t.Fatal("master key not decoded from hex as expected")
		}
	})
}

// TestLoadConfig_SMTPCertAndKeyTogether: STARTTLS needs both files, so one
// without the other is a configuration error, not a silently plaintext
// receiver.
func TestLoadConfig_SMTPCertAndKeyTogether(t *testing.T) {
	for _, args := range [][]string{
		{"-dev", "-smtp-cert", "/etc/ssl/mx.pem"},
		{"-dev", "-smtp-key", "/etc/ssl/mx.key"},
	} {
		if _, err := loadConfig(args); err == nil {
			t.Errorf("loadConfig(%v) = nil error, want one", args)
		}
	}
	cfg, err := loadConfig([]string{"-dev", "-smtp-cert", "/etc/ssl/mx.pem", "-smtp-key", "/etc/ssl/mx.key"})
	if err != nil || cfg.app.SMTPCert != "/etc/ssl/mx.pem" || cfg.app.SMTPKey != "/etc/ssl/mx.key" {
		t.Fatalf("both set: cfg=%q/%q err=%v", cfg.app.SMTPCert, cfg.app.SMTPKey, err)
	}
}

// TestProgramOptions pins where notify finds its dagnats: NOTIFY_DAGNATS_URL,
// else the deprecated -nats-url/NOTIFY_NATS_URL, else program.Run's default;
// and the seed beside the database unless NOTIFY_SEED names one.
func TestProgramOptions(t *testing.T) {
	t.Setenv("NOTIFY_MASTER_KEY_HEX", hex.EncodeToString(make([]byte, 32)))
	cases := []struct {
		name, dagnatsURL, natsURL, seed string
		args                            []string
		wantURL, wantSeed               string
	}{
		{
			name: "defaults", args: []string{"-db", "/data/notify.db"},
			wantURL: "nats://127.0.0.1:4222", wantSeed: "/data/notify.seed",
		},
		{
			name: "deprecated flag", args: []string{"-db", "/data/notify.db", "-nats-url", "nats://flag:4222"},
			wantURL: "nats://flag:4222", wantSeed: "/data/notify.seed",
		},
		{
			name: "deprecated env", natsURL: "nats://old:4222", args: []string{"-db", "/data/notify.db"},
			wantURL: "nats://old:4222", wantSeed: "/data/notify.seed",
		},
		{
			name: "NOTIFY_DAGNATS_URL wins", dagnatsURL: "nats://new:4222", seed: "/keys/notify.seed",
			args:    []string{"-db", "/data/notify.db", "-nats-url", "nats://flag:4222"},
			wantURL: "nats://new:4222", wantSeed: "/keys/notify.seed",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("NOTIFY_DAGNATS_URL", tc.dagnatsURL)
			t.Setenv("NOTIFY_NATS_URL", tc.natsURL)
			t.Setenv("NOTIFY_SEED", tc.seed)
			cfg, err := loadConfig(tc.args)
			if err != nil {
				t.Fatalf("loadConfig: %v", err)
			}
			opts := programOptions(cfg)
			if opts.URL != tc.wantURL || opts.SeedPath != tc.wantSeed {
				t.Fatalf("programOptions = url %q seed %q, want %q %q",
					opts.URL, opts.SeedPath, tc.wantURL, tc.wantSeed)
			}
			if len(opts.NATSOptions) != 3 {
				t.Fatalf("NATSOptions = %d, want notify's three connection handlers", len(opts.NATSOptions))
			}
		})
	}
}
