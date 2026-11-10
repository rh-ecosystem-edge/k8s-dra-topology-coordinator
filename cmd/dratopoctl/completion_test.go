package main

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	resourcev1 "k8s.io/api/resource/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	resourcev1client "k8s.io/client-go/kubernetes/typed/resource/v1"
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
	assert.Equal(t, []string{"gpu-full", "gpu-inference", "gpu-inference-rail0", "gpu-inference-rail1", "gpu-nic-pair"}, names)
	assert.NotContains(t, names, "native")
}

func TestCompleteManagedNamesTimesOutBlockedList(t *testing.T) {
	previousTimeout := completionTimeout
	completionTimeout = 10 * time.Millisecond
	t.Cleanup(func() { completionTimeout = previousTimeout })

	client := blockingKubernetesClient{Interface: fake.NewSimpleClientset()}
	start := time.Now()
	_, err := completeManagedNamesWithTimeout(context.Background(), client, "")

	require.ErrorIs(t, err, context.DeadlineExceeded)
	assert.Less(t, time.Since(start), 250*time.Millisecond)
}

type blockingKubernetesClient struct {
	kubernetes.Interface
}

func (c blockingKubernetesClient) ResourceV1() resourcev1client.ResourceV1Interface {
	return blockingResourceV1{ResourceV1Interface: c.Interface.ResourceV1()}
}

type blockingResourceV1 struct {
	resourcev1client.ResourceV1Interface
}

func (c blockingResourceV1) DeviceClasses() resourcev1client.DeviceClassInterface {
	return blockingDeviceClasses{}
}

type blockingDeviceClasses struct {
	resourcev1client.DeviceClassInterface
}

func (blockingDeviceClasses) List(ctx context.Context, _ metav1.ListOptions) (*resourcev1.DeviceClassList, error) {
	<-ctx.Done()
	return nil, ctx.Err()
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
