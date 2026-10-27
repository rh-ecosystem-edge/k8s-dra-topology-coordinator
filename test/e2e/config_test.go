//go:build e2e

package e2e

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLoadConfig_Defaults(t *testing.T) {
	cfg, err := loadConfig(func(string) string { return "" })
	require.NoError(t, err)

	assert.Equal(t, "nodepartition-e2e", cfg.Namespace)
	assert.Equal(t, "nodepartition-e2e", cfg.Release)
	assert.Equal(t, "full", cfg.Profile)
	assert.Equal(t, "nodepartition.dra.k8s.io", cfg.DriverName)
	assert.Equal(t, "cert-manager", cfg.TLSMode)
	assert.Equal(t, "selfsigned-issuer", cfg.TLSIssuerName)
	assert.Equal(t, "ClusterIssuer", cfg.TLSIssuerKind)
	assert.Equal(t, 5*time.Minute, cfg.Timeout)
	assert.False(t, cfg.DisposableCluster)
	assert.False(t, cfg.RequireCPU)
	assert.False(t, cfg.RequireKubeVirt)
}

func TestLoadConfig_ParsesProfileAndSafetyOptions(t *testing.T) {
	values := map[string]string{
		"E2E_NAMESPACE":          "e2e-ns",
		"E2E_RELEASE":            "e2e-release",
		"E2E_PROFILE":            "admission",
		"E2E_RUN_ID":             "run-123",
		"E2E_DRIVER_NAME":        "e2e.coordinator.example.com",
		"E2E_TLS_MODE":           "cert-manager",
		"E2E_TLS_ISSUER_NAME":    "e2e-issuer",
		"E2E_TLS_ISSUER_KIND":    "Issuer",
		"E2E_TIMEOUT":            "45s",
		"E2E_DISPOSABLE_CLUSTER": "true",
		"E2E_REQUIRE_CPU":        "1",
		"E2E_REQUIRE_KUBEVIRT":   "yes",
		"E2E_KUBECONFIG":         "/tmp/e2e-kubeconfig",
	}
	cfg, err := loadConfig(func(key string) string { return values[key] })
	require.NoError(t, err)

	assert.Equal(t, values["E2E_KUBECONFIG"], cfg.Kubeconfig)
	assert.Equal(t, values["E2E_RUN_ID"], cfg.RunID)
	assert.Equal(t, values["E2E_TLS_ISSUER_NAME"], cfg.TLSIssuerName)
	assert.Equal(t, values["E2E_TLS_ISSUER_KIND"], cfg.TLSIssuerKind)
	assert.Equal(t, 45*time.Second, cfg.Timeout)
	assert.True(t, cfg.DisposableCluster)
	assert.True(t, cfg.RequireCPU)
	assert.True(t, cfg.RequireKubeVirt)
}

func TestLoadConfig_RejectsInvalidValues(t *testing.T) {
	tests := []struct {
		name  string
		key   string
		value string
	}{
		{name: "profile", key: "E2E_PROFILE", value: "unknown"},
		{name: "namespace", key: "E2E_NAMESPACE", value: "not a namespace"},
		{name: "run id", key: "E2E_RUN_ID", value: "run id"},
		{name: "timeout", key: "E2E_TIMEOUT", value: "soon"},
		{name: "boolean", key: "E2E_REQUIRE_CPU", value: "sometimes"},
		{name: "TLS mode", key: "E2E_TLS_MODE", value: "unknown"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := map[string]string{tt.key: tt.value}
			_, err := loadConfig(func(key string) string { return cfg[key] })
			require.Error(t, err)
		})
	}
}

func TestLoadConfig_ManualTLSRequiresCABundle(t *testing.T) {
	_, err := loadConfig(func(key string) string {
		if key == "E2E_TLS_MODE" {
			return "manual"
		}
		return ""
	})
	require.Error(t, err)
}

func TestLoadConfig_AllowsCrossDriverProfile(t *testing.T) {
	cfg, err := loadConfig(func(key string) string {
		if key == "E2E_PROFILE" {
			return "cross-driver"
		}
		return ""
	})
	require.NoError(t, err)
	assert.Equal(t, "cross-driver", cfg.Profile)
}
