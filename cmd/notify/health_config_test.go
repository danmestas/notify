package main

import (
	"testing"
	"time"

	"github.com/danmestas/notify/internal/deliveryhealth"
)

func TestLoadConfig_HealthSettings(t *testing.T) {
	cfg, err := loadConfig([]string{"-dev"})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.app.Health != deliveryhealth.DefaultSettings() {
		t.Fatalf("default health = %+v", cfg.app.Health)
	}

	t.Setenv("NOTIFY_HEALTH_DEGRADED_RATE", "0.5")
	t.Setenv("NOTIFY_HEALTH_FAILING_ERRORS", "5")
	t.Setenv("NOTIFY_HEALTH_WINDOW", "30m")
	cfg, err = loadConfig([]string{"-dev"})
	if err != nil {
		t.Fatal(err)
	}
	want := deliveryhealth.Settings{DegradedRate: 0.5, FailingErrors: 5, Window: 30 * time.Minute}
	if cfg.app.Health != want {
		t.Fatalf("health = %+v, want %+v", cfg.app.Health, want)
	}
}

func TestLoadConfig_HealthSettingsRejectsBadValues(t *testing.T) {
	for name, kv := range map[string][2]string{
		"rate not a number": {"NOTIFY_HEALTH_DEGRADED_RATE", "lots"},
		"rate out of range": {"NOTIFY_HEALTH_DEGRADED_RATE", "1.5"},
		"errors zero":       {"NOTIFY_HEALTH_FAILING_ERRORS", "0"},
		"window no unit":    {"NOTIFY_HEALTH_WINDOW", "60"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv(kv[0], kv[1])
			if _, err := loadConfig([]string{"-dev"}); err == nil {
				t.Fatalf("%s=%s accepted", kv[0], kv[1])
			}
		})
	}
}
