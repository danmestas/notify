package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/danmestas/notify/internal/credverify"
	"github.com/danmestas/notify/internal/deliveryhealth"
)

// healthSettings reads the integration health thresholds (#100) from the
// environment. An unset variable keeps its default; a value that does not
// parse, or a set that cannot be evaluated, is an error so a typo stops start
// instead of silently using the default.
//
//	NOTIFY_HEALTH_DEGRADED_RATE   failure rate above which an integration is degraded (0.20)
//	NOTIFY_HEALTH_FAILING_ERRORS  failures with no success that make it failing (3)
//	NOTIFY_HEALTH_WINDOW          how far back results count, a Go duration (1h)
func healthSettings() (deliveryhealth.Settings, error) {
	set := deliveryhealth.DefaultSettings()
	if v := os.Getenv("NOTIFY_HEALTH_DEGRADED_RATE"); v != "" {
		f, err := strconv.ParseFloat(v, 64)
		if err != nil {
			return set, fmt.Errorf("NOTIFY_HEALTH_DEGRADED_RATE: %w", err)
		}
		set.DegradedRate = f
	}
	if v := os.Getenv("NOTIFY_HEALTH_FAILING_ERRORS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return set, fmt.Errorf("NOTIFY_HEALTH_FAILING_ERRORS: %w", err)
		}
		set.FailingErrors = n
	}
	if v := os.Getenv("NOTIFY_HEALTH_WINDOW"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return set, fmt.Errorf("NOTIFY_HEALTH_WINDOW: %w", err)
		}
		set.Window = d
	}
	if err := set.Validate(); err != nil {
		return set, err
	}
	return set, nil
}

// healthVerifyCron reads NOTIFY_HEALTH_VERIFY_CRON (#100): when the daily
// credential check runs, as a 5-field cron expression in UTC. Unset keeps the
// default (credverify.DefaultVerifyCron, 06:17 UTC); "off" turns the check
// off and removes its trigger. A malformed value is an error, so a typo stops
// start instead of silently skipping the check.
func healthVerifyCron() (string, error) {
	v := strings.TrimSpace(os.Getenv("NOTIFY_HEALTH_VERIFY_CRON"))
	if err := credverify.ValidateVerifyCron(v); err != nil {
		return "", fmt.Errorf("NOTIFY_HEALTH_VERIFY_CRON: %w", err)
	}
	return v, nil
}
