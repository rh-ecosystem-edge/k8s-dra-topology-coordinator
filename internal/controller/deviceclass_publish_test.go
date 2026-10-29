package controller

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	resourcev1 "k8s.io/api/resource/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func testDC(name, driver string, labels map[string]string, cel string) *resourcev1.DeviceClass {
	return &resourcev1.DeviceClass{
		ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels},
		Spec: resourcev1.DeviceClassSpec{
			Selectors: []resourcev1.DeviceSelector{{CEL: &resourcev1.CELDeviceSelector{Expression: cel}}},
			Config: []resourcev1.DeviceClassConfiguration{{
				DeviceConfiguration: resourcev1.DeviceConfiguration{
					Opaque: &resourcev1.OpaqueDeviceConfiguration{Driver: driver},
				},
			}},
		},
	}
}

func managedLabels(extra ...string) map[string]string {
	l := map[string]string{CoordinatorDriverName + "/managed": "true"}
	for i := 0; i+1 < len(extra); i += 2 {
		l[CoordinatorDriverName+"/"+extra[i]] = extra[i+1]
	}
	return l
}

func TestPublishDeviceClass_RefusesForeignClass(t *testing.T) {
	client := fake.NewSimpleClientset(testDC("x", "other", nil, "false"))
	m := NewDeviceClassManager(client, "drv", NewTopologyRuleStore())

	err := m.publishDeviceClass(context.Background(), testDC("x", "drv", managedLabels(), "true"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "refusing to overwrite")

	got, _ := client.ResourceV1().DeviceClasses().Get(context.Background(), "x", metav1.GetOptions{})
	assert.Equal(t, "false", got.Spec.Selectors[0].CEL.Expression)
}

func TestPublishDeviceClass_UpdatesManagedAndCreatesMissing(t *testing.T) {
	client := fake.NewSimpleClientset(testDC("x", "drv", managedLabels(), "false"))
	m := NewDeviceClassManager(client, "drv", NewTopologyRuleStore())
	ctx := context.Background()

	require.NoError(t, m.publishDeviceClass(ctx, testDC("x", "drv", managedLabels(), "true")))
	got, err := client.ResourceV1().DeviceClasses().Get(ctx, "x", metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, "true", got.Spec.Selectors[0].CEL.Expression)

	require.NoError(t, m.publishDeviceClass(ctx, testDC("y", "drv", managedLabels(), "true")))
	_, err = client.ResourceV1().DeviceClasses().Get(ctx, "y", metav1.GetOptions{})
	require.NoError(t, err)
}

func TestCleanupStale_SkipsOtherDriversClasses(t *testing.T) {
	ctx := context.Background()
	mk := func(name, driver string, extra ...string) *resourcev1.DeviceClass {
		return testDC(name, driver, managedLabels(extra...), "true")
	}
	client := fake.NewSimpleClientset(
		mk("mine-part", "drv", "partitionType", "t", "profile", "p"),
		mk("other-part", "other", "partitionType", "t", "profile", "p"),
		mk("mine-group", "drv", "grouping", "g"),
		mk("other-group", "other", "grouping", "g"),
	)
	m := NewDeviceClassManager(client, "drv", NewTopologyRuleStore())

	require.NoError(t, m.cleanupStaleDeviceClasses(ctx, map[string]bool{}, map[string]bool{}))
	require.NoError(t, m.cleanupStaleGroupingDeviceClasses(ctx, map[string]bool{}))

	list, err := client.ResourceV1().DeviceClasses().List(ctx, metav1.ListOptions{})
	require.NoError(t, err)
	var names []string
	for _, dc := range list.Items {
		names = append(names, dc.Name)
	}
	assert.ElementsMatch(t, []string{"other-part", "other-group"}, names)
}

func TestPublishDeviceClass_RefusesCrossKindOverwrite(t *testing.T) {
	ctx := context.Background()

	t.Run("grouping over partition", func(t *testing.T) {
		client := fake.NewSimpleClientset(testDC("x", "drv", managedLabels("partitionType", "full"), "false"))
		m := NewDeviceClassManager(client, "drv", NewTopologyRuleStore())
		err := m.publishDeviceClass(ctx, testDC("x", "drv", managedLabels("grouping", "x"), "true"))
		require.ErrorContains(t, err, "partition class")
	})

	t.Run("partition over grouping", func(t *testing.T) {
		client := fake.NewSimpleClientset(testDC("x", "drv", managedLabels("grouping", "x"), "false"))
		m := NewDeviceClassManager(client, "drv", NewTopologyRuleStore())
		err := m.publishDeviceClass(ctx, testDC("x", "drv", managedLabels("partitionType", "full"), "true"))
		require.ErrorContains(t, err, "grouping class")
	})
}
