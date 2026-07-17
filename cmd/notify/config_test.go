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
		if cfg.natsURL != "nats://127.0.0.1:4222" {
			t.Fatalf("natsURL default = %q, want nats://127.0.0.1:4222", cfg.natsURL)
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
		if string(cfg.masterKey) != string(want[:]) {
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
		if string(cfg.masterKey) != string(raw) {
			t.Fatal("master key not decoded from hex as expected")
		}
	})
}
