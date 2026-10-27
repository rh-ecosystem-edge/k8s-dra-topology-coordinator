//go:build e2e

package e2e

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/util/validation"
)

// Config contains the runtime contract for a live E2E profile.
type Config struct {
	Kubeconfig        string
	Namespace         string
	Release           string
	Profile           string
	RunID             string
	DriverName        string
	TLSMode           string
	TLSIssuerName     string
	TLSIssuerKind     string
	TLSCABundle       string
	Timeout           time.Duration
	DisposableCluster bool
	RequireCPU        bool
	RequireKubeVirt   bool
}

const defaultE2ETimeout = 5 * time.Minute

var validProfiles = map[string]bool{
	"controller":   true,
	"cross-driver": true,
	"admission":    true,
	"ha":           true,
	"full":         true,
	"kubevirt":     true,
}

func loadConfig(getenv func(string) string) (Config, error) {
	cfg := Config{
		Kubeconfig: getenv("E2E_KUBECONFIG"),
		Namespace:  valueOrDefault(getenv("E2E_NAMESPACE"), "nodepartition-e2e"),
		Release:    valueOrDefault(getenv("E2E_RELEASE"), "nodepartition-e2e"),
		Profile:    valueOrDefault(getenv("E2E_PROFILE"), "full"),
		RunID:      valueOrDefault(getenv("E2E_RUN_ID"), newRunID()),
		DriverName: valueOrDefault(getenv("E2E_DRIVER_NAME"), "nodepartition.dra.k8s.io"),
		TLSMode:    valueOrDefault(getenv("E2E_TLS_MODE"), "cert-manager"),
		TLSIssuerName: valueOrDefault(getenv("E2E_TLS_ISSUER_NAME"),
			"selfsigned-issuer"),
		TLSIssuerKind: valueOrDefault(getenv("E2E_TLS_ISSUER_KIND"), "ClusterIssuer"),
		TLSCABundle:   getenv("E2E_TLS_CA_BUNDLE"),
		Timeout:       defaultE2ETimeout,
	}

	if raw := getenv("E2E_TIMEOUT"); raw != "" {
		timeout, err := time.ParseDuration(raw)
		if err != nil || timeout <= 0 {
			return Config{}, fmt.Errorf("invalid E2E_TIMEOUT %q", raw)
		}
		cfg.Timeout = timeout
	}

	var err error
	if cfg.DisposableCluster, err = parseBool(getenv("E2E_DISPOSABLE_CLUSTER"), false); err != nil {
		return Config{}, fmt.Errorf("E2E_DISPOSABLE_CLUSTER: %w", err)
	}
	if cfg.RequireCPU, err = parseBool(getenv("E2E_REQUIRE_CPU"), false); err != nil {
		return Config{}, fmt.Errorf("E2E_REQUIRE_CPU: %w", err)
	}
	if cfg.RequireKubeVirt, err = parseBool(getenv("E2E_REQUIRE_KUBEVIRT"), false); err != nil {
		return Config{}, fmt.Errorf("E2E_REQUIRE_KUBEVIRT: %w", err)
	}

	if !validProfiles[cfg.Profile] {
		return Config{}, fmt.Errorf("invalid E2E_PROFILE %q", cfg.Profile)
	}
	if cfg.TLSMode != "cert-manager" && cfg.TLSMode != "openshift" && cfg.TLSMode != "manual" {
		return Config{}, fmt.Errorf("invalid E2E_TLS_MODE %q", cfg.TLSMode)
	}
	if cfg.TLSMode == "manual" && cfg.TLSCABundle == "" {
		return Config{}, fmt.Errorf("E2E_TLS_CA_BUNDLE is required for manual TLS mode")
	}
	for name, value := range map[string]string{
		"E2E_NAMESPACE": cfg.Namespace,
		"E2E_RELEASE":   cfg.Release,
		"E2E_RUN_ID":    cfg.RunID,
	} {
		if errs := validation.IsDNS1123Label(value); len(errs) > 0 {
			return Config{}, fmt.Errorf("%s %q is not a DNS label: %s", name, value, strings.Join(errs, "; "))
		}
	}
	if cfg.RunID == "" {
		return Config{}, fmt.Errorf("E2E_RUN_ID must not be empty")
	}

	return cfg, nil
}

func valueOrDefault(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}

func parseBool(value string, fallback bool) (bool, error) {
	if value == "" {
		return fallback, nil
	}
	switch strings.ToLower(value) {
	case "1", "true", "yes", "y":
		return true, nil
	case "0", "false", "no", "n":
		return false, nil
	default:
		return false, fmt.Errorf("expected true/false, got %q", value)
	}
}

func newRunID() string {
	return "run-" + strconv.FormatInt(time.Now().UnixNano(), 36)
}

func liveConfig(getenv func(string) string) (Config, error) {
	cfg, err := loadConfig(getenv)
	if err != nil {
		return Config{}, err
	}
	if cfg.Kubeconfig == "" {
		return Config{}, fmt.Errorf("E2E_KUBECONFIG is required for live E2E")
	}
	if !cfg.DisposableCluster {
		return Config{}, fmt.Errorf("E2E_DISPOSABLE_CLUSTER must be true for live E2E")
	}
	return cfg, nil
}

func currentLiveConfig() (Config, error) {
	return liveConfig(os.Getenv)
}
