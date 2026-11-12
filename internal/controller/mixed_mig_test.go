package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	resourcev1 "k8s.io/api/resource/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func mixedMIGDevices(secondStart int, splitGPU bool) []TopologyDevice {
	devices := []TopologyDevice{
		migDevice(0, "gpu-0-4g", "mig", "4g.40gb", 0, 4),
		migDevice(0, "gpu-0-2g", "mig", "2g.20gb", secondStart, 2),
		migDevice(0, "gpu-0-1g", "mig", "1g.10gb", 6, 1),
	}
	limits := map[string]int64{"memory": 8 * 5 * migGiB, "multiprocessors": 8 * migSMs}
	for i := 0; i < 8; i++ {
		limits["memory-slice-"+string(rune('0'+i))] = 1
	}
	for i := range devices {
		devices[i].PCIBusID = strPtr("0000:4e:00.0")
		devices[i].PCIeRoot = strPtr("pci0000:48")
		devices[i].CounterCapacity = map[string]map[string]int64{"gpu-0-counter-set": limits}
	}
	if splitGPU {
		for i := 1; i < len(devices); i++ {
			devices[i].PCIBusID = strPtr("0000:9a:00.0")
			devices[i].ConsumesCounters[0].CounterSet = "gpu-1-counter-set"
			devices[i].CounterCapacity = map[string]map[string]int64{"gpu-1-counter-set": limits}
		}
	}
	return devices
}

func mixedMIGGrouping() DeviceGrouping {
	return DeviceGrouping{Name: "mixed-mig", Alignment: "pcieRoot", MatchAttribute: AttrPCIBusID, Devices: []GroupingDevice{
		{Name: "large", Class: "mig.nvidia.com", Profile: "4g.40gb", Count: 1},
		{Name: "medium", Class: "mig.nvidia.com", Profile: "2g.20gb", Count: 1},
		{Name: "small", Class: "mig.nvidia.com", Profile: "1g.10gb", Count: 1},
	}}
}

func TestMixedMIGGroupingUsesOneCounterSet(t *testing.T) {
	for _, tc := range []struct {
		name    string
		start   int
		split   bool
		limit   int64
		missing bool
		want    int
	}{
		{"fits", 4, false, 0, false, 1}, {"overlapping slices", 0, false, 0, false, 0},
		{"two GPUs one PCIe root", 4, true, 0, false, 0},
		{"over capacity", 4, false, 6 * 5 * migGiB, false, 0},
		{"missing capacity", 4, false, 0, true, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			model := NewTopologyModel()
			devices := mixedMIGDevices(tc.start, tc.split)
			if tc.limit != 0 {
				for i := range devices {
					devices[i].CounterCapacity["gpu-0-counter-set"]["memory"] = tc.limit
				}
			}
			if tc.missing {
				devices[0].CounterCapacity = nil
			}
			model.nodes["node-1"] = &NodeTopology{NodeName: "node-1", DevicesByDriver: map[string][]TopologyDevice{migDriver: devices}}
			rules := migRules(t)
			result := NewGroupingBuilder(model, rules).BuildGroupings([]DeviceGrouping{mixedMIGGrouping()})
			if tc.want == 0 {
				assert.Empty(t, result)
				return
			}
			require.Len(t, result, 1)
			require.Len(t, result[0].Instances, 1)
			config := NewDeviceClassManager(nil, "", rules).buildGroupingAggregateConfig(result[0].Instances[0])
			assert.Equal(t, PartitionConfigV1Alpha2, config.APIVersion)
			require.Len(t, config.SubResources, 3)
			assert.Contains(t, config.Alignments, AlignmentConfig{
				Attribute: AttrPCIBusID, Requests: []string{"large", "medium", "small"},
				Enforcement: EnforcementRequired, Scope: "group",
			})
			assert.Contains(t, config.Alignments, AlignmentConfig{
				Attribute: AttrPCIeRoot, Requests: []string{"large", "medium", "small"},
				Enforcement: EnforcementRequired,
			})
			assert.NoError(t, config.Validate())
			client := fake.NewSimpleClientset()
			manager := NewDeviceClassManager(client, CoordinatorDriverName, rules)
			require.NoError(t, manager.SyncGroupingDeviceClasses(context.Background(), result))
			dc, err := client.ResourceV1().DeviceClasses().Get(context.Background(), "mixed-mig", metav1.GetOptions{})
			require.NoError(t, err, "the aggregate mixed-profile offering must be published")
			var published PartitionConfig
			require.NoError(t, json.Unmarshal(dc.Spec.Config[0].Opaque.Parameters.Raw, &published))
			assert.Equal(t, PartitionConfigV1Alpha2, published.APIVersion)
			assert.Len(t, published.SubResources, 3)
		})
	}
}

func TestMixedMIGGroupingHandlesMultipleCounterSets(t *testing.T) {
	devices := mixedMIGDevices(4, false)
	for i := range devices {
		consumed := devices[i].ConsumesCounters[0]
		localSet := fmt.Sprintf("local-%d", i)
		sets := []resourcev1.DeviceCounterConsumption{
			{CounterSet: localSet, Counters: map[string]resourcev1.Counter{"local": makeCounter(1)}},
			{CounterSet: "gpu-0-counter-set", Counters: consumed.Counters},
			{CounterSet: "gpu-0-secondary-counter-set", Counters: consumed.Counters},
		}
		switch i {
		case 1:
			sets[0], sets[1] = sets[1], sets[0]
		case 2:
			sets[0], sets[2] = sets[2], sets[0]
		}
		devices[i].ConsumesCounters = sets
		devices[i].CounterCapacity[localSet] = map[string]int64{"local": 1}
		devices[i].CounterCapacity["gpu-0-secondary-counter-set"] = devices[i].CounterCapacity["gpu-0-counter-set"]
	}
	model := NewTopologyModel()
	model.nodes["node-1"] = &NodeTopology{NodeName: "node-1", DevicesByDriver: map[string][]TopologyDevice{migDriver: devices}}

	results := NewGroupingBuilder(model, migRules(t)).BuildGroupings([]DeviceGrouping{mixedMIGGrouping()})
	require.Len(t, results, 1)
	require.Len(t, results[0].Instances, 1, "the common counter sets describe one identical bundle")
	assert.Equal(t, 3, len(results[0].Instances[0].Devices))
}

func TestMixedMIGGroupingConfigNames(t *testing.T) {
	store := NewGroupingStore()
	err := store.LoadFromConfigMap(makeGroupingConfigMap("mixed-mig", map[string]string{
		"name": "mixed-mig", "alignment": "pcieRoot", "matchAttribute": AttrPCIBusID,
		"devices": "- name: large\n  class: mig.nvidia.com\n  profile: 4g.40gb\n  count: 1\n- name: small\n  class: mig.nvidia.com\n  profile: 1g.10gb\n  count: 1\n",
	}))
	require.NoError(t, err)
	groupings := store.GetGroupings()
	require.Len(t, groupings, 1)
	assert.Equal(t, AttrPCIBusID, groupings[0].MatchAttribute)
	assert.Equal(t, "large", groupings[0].Devices[0].Name)
	assert.Equal(t, "small", groupings[0].Devices[1].Name)
}

func TestMixedMIGGroupingRejectsImpossibleCountBeforeAllocatingSlots(t *testing.T) {
	model := NewTopologyModel()
	model.nodes["node-1"] = &NodeTopology{NodeName: "node-1", DevicesByDriver: map[string][]TopologyDevice{migDriver: mixedMIGDevices(4, false)}}
	grouping := mixedMIGGrouping()
	grouping.Devices[0].Count = 1 << 30
	assert.Empty(t, NewGroupingBuilder(model, migRules(t)).BuildGroupings([]DeviceGrouping{grouping}))
}
