package main

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	resourcev1 "k8s.io/api/resource/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/rh-ecosystem-edge/k8s-dra-topology-coordinator/internal/controller"
)

func class(name string, labels, annotations map[string]string) resourcev1.DeviceClass {
	return resourcev1.DeviceClass{
		ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels, Annotations: annotations},
		Spec: resourcev1.DeviceClassSpec{
			Selectors: []resourcev1.DeviceSelector{{CEL: &resourcev1.CELDeviceSelector{Expression: "secret-selector"}}},
			Config: []resourcev1.DeviceClassConfiguration{{
				DeviceConfiguration: resourcev1.DeviceConfiguration{
					Opaque: &resourcev1.OpaqueDeviceConfiguration{Driver: "drv"},
				},
			}},
		},
	}
}

func fixtureClasses() []resourcev1.DeviceClass {
	managed := func(kv ...string) map[string]string {
		l := map[string]string{controller.LabelManaged: "true"}
		for i := 0; i+1 < len(kv); i += 2 {
			l[kv[i]] = kv[i+1]
		}
		return l
	}
	return []resourcev1.DeviceClass{
		class("gpu-inference", managed(
			controller.LabelGrouping, "gpu-inference", controller.LabelAggregate, "true",
			controller.LabelOffering, "true", controller.LabelMode, "container",
			controller.LabelSource, controller.SourceConfigMap, controller.LabelAlignment, "pcieRoot",
			controller.LabelIntentLevel, "workload", controller.LabelVisibility, "public",
			controller.LabelCount, "4"),
			map[string]string{
				controller.AnnotationDisplayName: "GPU inference",
				controller.AnnotationCategories:  "ai,inference",
			}),
		class("gpu-inference-rail0", managed(
			controller.LabelGrouping, "gpu-inference", controller.LabelSource, controller.SourceConfigMap), nil),
		class("gpu-inference-rail1", managed(
			controller.LabelGrouping, "gpu-inference", controller.LabelSource, controller.SourceConfigMap), nil),
		class("plain", managed(
			controller.LabelGrouping, "plain", controller.LabelAggregate, "true",
			controller.LabelSource, controller.SourceConfigMap), nil),
		class("gpu-nic-pair", managed(
			controller.LabelGrouping, "gpu-nic-pair", controller.LabelAggregate, "true",
			controller.LabelSource, controller.SourceAutoDetected), nil),
		class("gpu-full", managed(controller.LabelPartitionType, "full"), nil),
		class("native", map[string]string{}, nil),
	}
}

func TestBuildEntries_KindsAndVariants(t *testing.T) {
	entries := buildEntries(fixtureClasses())

	kinds := map[string]string{}
	for _, e := range entries {
		kinds[e.Name] = e.Kind
	}
	assert.Equal(t, map[string]string{
		"gpu-inference": kindOffering,
		"plain":         kindGrouping,
		"gpu-nic-pair":  kindAutoDetected,
		"gpu-full":      kindPartition,
	}, kinds, "variants and unmanaged classes are not entries")

	for _, e := range entries {
		switch e.Name {
		case "gpu-inference":
			assert.Equal(t, []string{"gpu-inference-rail0", "gpu-inference-rail1"}, e.Variants)
			assert.Equal(t, 4, e.Count)
			assert.Equal(t, []string{"ai", "inference"}, e.Categories)
			assert.Equal(t, "GPU inference", e.DisplayName)
		case "plain":
			assert.Equal(t, 1, e.Count, "missing count label means one instance")
			assert.Empty(t, e.Variants)
		}
	}
}

func TestFilterEntries(t *testing.T) {
	entries := buildEntries(fixtureClasses())
	curated := filterEntries(entries, false)
	require.Len(t, curated, 1)
	assert.Equal(t, "gpu-inference", curated[0].Name)
	assert.Len(t, filterEntries(entries, true), 4)
}

func TestRunList_NeverPrintsClassConfig(t *testing.T) {
	client := fake.NewSimpleClientset()
	for _, dc := range fixtureClasses() {
		dc := dc
		_, err := client.ResourceV1().DeviceClasses().Create(context.Background(), &dc, metav1.CreateOptions{})
		require.NoError(t, err)
	}

	for _, format := range []string{"table", "json"} {
		var buf bytes.Buffer
		require.NoError(t, runList(context.Background(), client, &buf, true, format))
		assert.NotContains(t, buf.String(), "secret-selector")
		assert.NotContains(t, buf.String(), "drv")
		assert.Contains(t, buf.String(), "gpu-inference")
		assert.NotContains(t, buf.String(), "native")
	}

	var buf bytes.Buffer
	require.NoError(t, runList(context.Background(), client, &buf, false, "json"))
	var got []Entry
	require.NoError(t, json.Unmarshal(buf.Bytes(), &got))
	require.Len(t, got, 1)
	assert.Equal(t, kindOffering, got[0].Kind)
}

func TestRunGet(t *testing.T) {
	client := fake.NewSimpleClientset()
	for _, dc := range fixtureClasses() {
		dc := dc
		_, err := client.ResourceV1().DeviceClasses().Create(context.Background(), &dc, metav1.CreateOptions{})
		require.NoError(t, err)
	}

	var buf bytes.Buffer
	require.NoError(t, runGet(context.Background(), client, &buf, "gpu-full", "table"))
	assert.Contains(t, buf.String(), kindPartition)

	err := runGet(context.Background(), client, &buf, "missing", "table")
	require.ErrorContains(t, err, `no managed DeviceClass "missing"`)
	require.ErrorContains(t, runGet(context.Background(), client, &buf, "gpu-full", "yaml"), "invalid --output")
}
