package main

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func TestCompleteManagedNamesUsesCatalogEntries(t *testing.T) {
	client := fake.NewSimpleClientset()
	for _, dc := range fixtureClasses() {
		dc := dc
		_, err := client.ResourceV1().DeviceClasses().Create(context.Background(), &dc, metav1.CreateOptions{})
		require.NoError(t, err)
	}

	names, err := completeManagedNames(context.Background(), client, "gpu-")
	require.NoError(t, err)
	assert.Equal(t, []string{"gpu-full", "gpu-inference", "gpu-nic-pair"}, names)
	assert.NotContains(t, names, "gpu-inference-rail0")
	assert.NotContains(t, names, "native")
}

func TestRootCommandRegistersNameCompletion(t *testing.T) {
	root := newRootCmd()
	completion, _, err := root.Find([]string{"completion"})
	require.NoError(t, err)
	assert.Equal(t, "completion", completion.Name())

	get, _, err := root.Find([]string{"offerings", "get"})
	require.NoError(t, err)
	assert.NotNil(t, get.ValidArgsFunction)

	explain, _, err := root.Find([]string{"offerings", "explain"})
	require.NoError(t, err)
	assert.NotNil(t, explain.ValidArgsFunction)
}

func TestCompleteManagedNamesUsesManagedLabel(t *testing.T) {
	client := fake.NewSimpleClientset()
	dc := class("native", map[string]string{}, nil)
	_, err := client.ResourceV1().DeviceClasses().Create(context.Background(), &dc, metav1.CreateOptions{})
	require.NoError(t, err)

	names, err := completeManagedNames(context.Background(), client, "")
	require.NoError(t, err)
	assert.Empty(t, names)
}
