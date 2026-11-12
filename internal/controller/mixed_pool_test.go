package controller

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	resourcev1 "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

func TestTopologyModelUsesCompleteCounterPoolGeneration(t *testing.T) {
	device := makeGPUDevice("mig", 0, "pci-0")
	device.Attributes[resourcev1.QualifiedName(AttrPCIBusID)] = resourcev1.DeviceAttribute{StringValue: strPtr("0000:4e:00.0")}
	device.ConsumesCounters = []resourcev1.DeviceCounterConsumption{{CounterSet: "gpu-0", Counters: map[string]resourcev1.Counter{"memory": {Value: resource.MustParse("10Gi")}}}}
	first := makeResourceSlice("first", migDriver, "node-1", "pool", []resourcev1.Device{device})
	first.Spec.Pool.ResourceSliceCount = 2
	first.Spec.SharedCounters = []resourcev1.CounterSet{{Name: "gpu-0", Counters: map[string]resourcev1.Counter{"memory": {Value: resource.MustParse("80Gi")}}}}
	second := makeResourceSlice("second", migDriver, "node-1", "pool", nil)
	second.Spec.Pool.ResourceSliceCount = 2
	model := NewTopologyModel()
	model.UpdateFromResourceSlice(first)
	assert.Nil(t, model.GetNodeTopology("node-1"))
	model.UpdateFromResourceSlice(second)
	node := model.GetNodeTopology("node-1")
	require.NotNil(t, node)
	require.Len(t, node.AllDevices(), 1)
	assert.Equal(t, "0000:4e:00.0", *node.AllDevices()[0].PCIBusID)
	capacity := resource.MustParse("80Gi")
	assert.Equal(t, capacity.Value(), node.AllDevices()[0].CounterCapacity["gpu-0"]["memory"])

	newFirst := first.DeepCopy()
	newFirst.Spec.Pool.Generation = 2
	model.UpdateFromResourceSlice(newFirst)
	assert.Nil(t, model.GetNodeTopology("node-1"), "an incomplete newer generation must not use stale devices")
	newSecond := second.DeepCopy()
	newSecond.Spec.Pool.Generation = 2
	newSecond.Spec.SharedCounters = []resourcev1.CounterSet{{Name: "gpu-0", Counters: map[string]resourcev1.Counter{"memory": {Value: resource.MustParse("40Gi")}}}}
	model.UpdateFromResourceSlice(newSecond)
	assert.Nil(t, model.GetNodeTopology("node-1"), "conflicting counter capacities must fail closed")
	newSecond.Spec.SharedCounters = nil
	model.UpdateFromResourceSlice(newSecond)
	require.NotNil(t, model.GetNodeTopology("node-1"))
}
