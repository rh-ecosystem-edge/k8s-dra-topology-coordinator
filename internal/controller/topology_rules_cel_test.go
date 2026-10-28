package controller

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBuildNUMACELSelector_RejectsUnsafeInput(t *testing.T) {
	for _, attr := range []string{
		`evil".x/numaNode`,
		`gpu.amd.com/num aNode`,
		`gpu.amd.com/numaNode)||true`,
		`gpu.amd.com/1numa`,
		`gpu.amd.com/`,
		`/numaNode`,
	} {
		assert.Equal(t, "", BuildNUMACELSelector(attr, []int64{0}), attr)
	}
}

func TestBuildNUMACELSelector_EmptyValues(t *testing.T) {
	assert.Equal(t, "", BuildNUMACELSelector("gpu.amd.com/numaNode", nil))
}

func TestParseTopologyRule_NUMANodeAttributeValidation(t *testing.T) {
	base := func(attr string) map[string]string {
		return map[string]string{"attribute": attr, "type": "int", "driver": "gpu.amd.com", "mapsTo": "numaNode"}
	}
	store := NewTopologyRuleStore()
	err := store.LoadFromConfigMap(makeTopologyRuleConfigMap("bad", "default", base(`gpu.amd.com/numa)Node`)))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid attribute")

	require.NoError(t, store.LoadFromConfigMap(makeTopologyRuleConfigMap("ok", "default", base("numaNode"))))
	attr, ok := store.GetNUMAAttributeForDriver("gpu.amd.com")
	require.True(t, ok)
	assert.Equal(t, "numaNode", attr)
}
