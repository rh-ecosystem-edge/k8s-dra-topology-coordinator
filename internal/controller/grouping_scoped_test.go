package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	resourcev1 "k8s.io/api/resource/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func scopedCM() *corev1.ConfigMap {
	return &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "local", Labels: map[string]string{DeviceGroupingLabel: "true"}}, Data: map[string]string{
		"name": "local", "offering": "true", "devices": `- name: gpu
  class: gpu.example
  count: 1
- name: nic
  class: nic.example
  count: 1
- name: cpu
  class: cpu.example
  count: 1
- name: memory
  class: memory.example
  count: 1`, "alignments": `- attribute: resource.kubernetes.io/pcieRoot
  requests: [gpu, nic]
- attribute: resource.kubernetes.io/numaNode
  requests: [gpu, cpu, memory]`}}
}

func TestScopedGroupingConfig(t *testing.T) {
	store := NewGroupingStore()
	require.NoError(t, store.LoadFromConfigMap(scopedCM()))
	require.Len(t, store.GetGroupings(), 1)
}

func scopedTopology(t *testing.T, root string, numa int64) (*TopologyModel, DeviceGrouping) {
	t.Helper()
	g, err := parseDeviceGrouping(scopedCM())
	require.NoError(t, err)
	model := NewTopologyModel()
	for _, entry := range g.Devices {
		attrs := map[resourcev1.QualifiedName]resourcev1.DeviceAttribute{}
		if entry.Name == "gpu" || entry.Name == "nic" {
			attrs[resourcev1.QualifiedName(AttrPCIeRoot)] = resourcev1.DeviceAttribute{StringValue: strPtr("root-a")}
		}
		if entry.Name != "nic" {
			attrs[resourcev1.QualifiedName(AttrNUMANode)] = resourcev1.DeviceAttribute{IntValue: intPtr(0)}
		}
		if entry.Name == "nic" {
			attrs[resourcev1.QualifiedName(AttrPCIeRoot)] = resourcev1.DeviceAttribute{StringValue: strPtr(root)}
		}
		if entry.Name == "cpu" {
			attrs[resourcev1.QualifiedName(AttrNUMANode)] = resourcev1.DeviceAttribute{IntValue: intPtr(numa)}
		}
		model.UpdateFromResourceSlice(makeResourceSlice(entry.Name, entry.Class, "node-1", "pool", []resourcev1.Device{{Name: entry.Name, Attributes: attrs}}))
	}
	return model, g
}

func TestScopedGroupingJointTopology(t *testing.T) {
	for _, tc := range []struct {
		name, root string
		numa       int64
		want       int
	}{
		{"local NIC without NUMA", "root-a", 0, 1}, {"different roots", "root-b", 0, 0}, {"different NUMA", "root-a", 1, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			model, g := scopedTopology(t, tc.root, tc.numa)
			got := NewGroupingBuilder(model, NewTopologyRuleStore()).BuildGroupings([]DeviceGrouping{g})
			require.Len(t, got, tc.want)
		})
	}
}

func TestScopedGroupingPublishesOnlyAggregate(t *testing.T) {
	model, g := scopedTopology(t, "root-a", 0)
	results := NewGroupingBuilder(model, NewTopologyRuleStore()).BuildGroupings([]DeviceGrouping{g})
	client := fake.NewSimpleClientset()
	require.NoError(t, NewDeviceClassManager(client, CoordinatorDriverName, NewTopologyRuleStore()).SyncGroupingDeviceClasses(context.Background(), results))
	classes, err := client.ResourceV1().DeviceClasses().List(context.Background(), metav1.ListOptions{})
	require.NoError(t, err)
	require.Len(t, classes.Items, 1)
	dc := classes.Items[0]
	require.Equal(t, "local", dc.Name)
	require.Equal(t, "mixed", dc.Labels[LabelAlignment])
	var config PartitionConfig
	require.NoError(t, json.Unmarshal(dc.Spec.Config[0].Opaque.Parameters.Raw, &config))
	require.Equal(t, CoordinatorDriverName+"/v1alpha3", config.APIVersion)
	require.Len(t, config.Alignments, 2)
	require.NoError(t, config.Validate())
	for _, a := range config.Alignments {
		require.Equal(t, "group", a.Scope)
		require.Equal(t, EnforcementRequired, a.Enforcement)
	}
	for _, sr := range config.SubResources {
		require.Empty(t, sr.Selectors)
	}
}

func TestScopedGroupingParserValidationAndCopy(t *testing.T) {
	for _, tc := range []struct{ name, key, value string }{
		{"legacy", "alignment", "numaNode"}, {"fallback", "fallback", "numaNode"}, {"match", "matchAttribute", AttrNUMANode}, {"rail", "rail", "true"},
		{"empty", "alignments", "[]"}, {"attribute", "alignments", "- attribute: wrong\n  requests: [gpu]"}, {"unknown ref", "alignments", "- attribute: " + AttrPCIeRoot + "\n  requests: [other]"}, {"duplicate ref", "alignments", "- attribute: " + AttrPCIeRoot + "\n  requests: [gpu,gpu]"}, {"no refs", "alignments", "- attribute: " + AttrPCIeRoot}, {"unknown key", "alignments", "- attribute: " + AttrPCIeRoot + "\n  requests: [gpu]\n  typo: true"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cm := scopedCM()
			cm.Data[tc.key] = tc.value
			_, err := parseDeviceGrouping(cm)
			require.Error(t, err)
		})
	}
	store := NewGroupingStore()
	require.NoError(t, store.LoadFromConfigMap(scopedCM()))
	first := store.GetGroupings()
	first[0].Alignments[0].Requests[0] = "changed"
	require.NotEqual(t, "changed", store.GetGroupings()[0].Alignments[0].Requests[0])
}

func TestScopedMatcherBacktracksAndRequiresNativeAttributes(t *testing.T) {
	model, g := scopedTopology(t, "root-a", 0)
	bad := resourcev1.Device{Name: "a-bad", Attributes: map[resourcev1.QualifiedName]resourcev1.DeviceAttribute{resourcev1.QualifiedName(AttrNUMANode): {IntValue: intPtr(0)}, resourcev1.QualifiedName(AttrPCIeRoot): {StringValue: strPtr("other")}}}
	good := *bad.DeepCopy()
	good.Name = "z-good"
	good.Attributes[resourcev1.QualifiedName(AttrPCIeRoot)] = resourcev1.DeviceAttribute{StringValue: strPtr("root-a")}
	model.UpdateFromResourceSlice(makeResourceSlice("gpu", "gpu.example", "node-1", "pool", []resourcev1.Device{bad, good}))
	got := NewGroupingBuilder(model, NewTopologyRuleStore()).BuildGroupings([]DeviceGrouping{g})
	require.Len(t, got, 1)
	require.Equal(t, "z-good", got[0].Instances[0].Devices[0].DeviceName)
	good.Attributes[resourcev1.QualifiedName(AttrNUMANode)] = resourcev1.DeviceAttribute{StringValue: strPtr("0")}
	model.UpdateFromResourceSlice(makeResourceSlice("gpu", "gpu.example", "node-1", "pool", []resourcev1.Device{good}))
	require.Empty(t, NewGroupingBuilder(model, NewTopologyRuleStore()).BuildGroupings([]DeviceGrouping{g}))
}

func TestScopedMatcherPublishedTopologyAndCopy(t *testing.T) {
	model, g := scopedTopology(t, "root-a", 0)
	model.SetRules([]TopologyRule{{Driver: "gpu.example", Attribute: "gpu.example/localNUMA", MapsTo: MapsToNUMANode}})
	attrs := map[resourcev1.QualifiedName]resourcev1.DeviceAttribute{resourcev1.QualifiedName(AttrNUMANode): {IntValue: intPtr(0)}, resourcev1.QualifiedName(AttrPCIeRoot): {StringValue: strPtr("root-a")}, "gpu.example/localNUMA": {IntValue: intPtr(7)}}
	model.UpdateFromResourceSlice(makeResourceSlice("gpu", "gpu.example", "node-1", "pool", []resourcev1.Device{{Name: "gpu", Attributes: attrs}}))
	require.Len(t, NewGroupingBuilder(model, NewTopologyRuleStore()).BuildGroupings([]DeviceGrouping{g}), 1)
	node := model.GetNodeTopology("node-1")
	for _, d := range node.AllDevices() {
		if d.DriverName == "gpu.example" {
			*d.PublishedTopology[AttrNUMANode].IntValue = 8
		}
	}
	require.Len(t, NewGroupingBuilder(model, NewTopologyRuleStore()).BuildGroupings([]DeviceGrouping{g}), 1)
	delete(attrs, resourcev1.QualifiedName(AttrNUMANode))
	model.UpdateFromResourceSlice(makeResourceSlice("gpu", "gpu.example", "node-1", "pool", []resourcev1.Device{{Name: "gpu", Attributes: attrs}}))
	require.Empty(t, NewGroupingBuilder(model, NewTopologyRuleStore()).BuildGroupings([]DeviceGrouping{g}))
}

func TestScopedMatcherExactNamespacedAndSecondaryCounters(t *testing.T) {
	for _, tc := range []struct {
		name            string
		shared, missing bool
		cost, limit     string
		want            int
	}{
		{"fractional overcommit", true, false, "0.6", "1", 0}, {"fractional fits", true, false, "0.4", "1", 1}, {"missing capacity", true, true, "1", "2", 0}, {"driver namespaces", false, false, "1", "1", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			model, g := scopedTopology(t, "root-a", 0)
			g.Devices = g.Devices[:2]
			g.Alignments = []GroupingAlignment{{Attribute: AttrPCIeRoot, Requests: []string{"gpu", "nic"}}}
			for i, entry := range g.Devices {
				driver := entry.Class
				if tc.shared {
					driver = "shared.example"
				}
				dev := resourcev1.Device{Name: entry.Name, Attributes: map[resourcev1.QualifiedName]resourcev1.DeviceAttribute{resourcev1.QualifiedName(AttrPCIeRoot): {StringValue: strPtr("root-a")}}}
				primary := fmt.Sprintf("primary-%d", i)
				cc := func(set string) resourcev1.DeviceCounterConsumption {
					return resourcev1.DeviceCounterConsumption{CounterSet: set, Counters: map[string]resourcev1.Counter{"units": {Value: resource.MustParse(tc.cost)}}}
				}
				dev.ConsumesCounters = []resourcev1.DeviceCounterConsumption{cc(primary), cc("secondary")}
				slice := makeResourceSlice(entry.Name, driver, "node-1", "pool", []resourcev1.Device{dev})
				slice.Spec.SharedCounters = []resourcev1.CounterSet{{Name: primary, Counters: map[string]resourcev1.Counter{"units": {Value: resource.MustParse(tc.limit)}}}}
				if !tc.missing {
					slice.Spec.SharedCounters = append(slice.Spec.SharedCounters, resourcev1.CounterSet{Name: "secondary", Counters: map[string]resourcev1.Counter{"units": {Value: resource.MustParse(tc.limit)}}})
				}
				if tc.shared {
					slice.Spec.Pool.ResourceSliceCount = 2
				}
				model.UpdateFromResourceSlice(slice)
			}
			rules := NewTopologyRuleStore()
			if tc.shared {
				// Classes remain distinct while sharing the same driver/pool counter namespace.
				for _, raw := range model.rawSlices {
					if raw.DriverName != "shared.example" {
						continue
					}
					for j := range raw.Devices {
						raw.Devices[j].Attributes["shared.example/kind"] = resourcev1.DeviceAttribute{StringValue: strPtr(raw.SliceName)}
					}
					model.rawSlices[raw.SliceName] = raw
				}
				require.NoError(t, rules.LoadFromConfigMap(&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "gpu", Labels: map[string]string{TopologyRuleLabel: "true"}}, Data: map[string]string{"attribute": "shared.example/kind", "driver": "shared.example", "type": "string", "deviceClass": "gpu.example", "whenValue": "gpu"}}))
				require.NoError(t, rules.LoadFromConfigMap(&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "nic", Labels: map[string]string{TopologyRuleLabel: "true"}}, Data: map[string]string{"attribute": "shared.example/kind", "driver": "shared.example", "type": "string", "deviceClass": "nic.example", "whenValue": "nic"}}))
				model.SetRules(rules.GetRules())
			}
			require.Len(t, NewGroupingBuilder(model, rules).BuildGroupings([]DeviceGrouping{g}), tc.want)
		})
	}
}

func TestScopedConversionPreservesVariantsUntilAggregatePublishes(t *testing.T) {
	model, g := scopedTopology(t, "root-a", 0)
	results := NewGroupingBuilder(model, NewTopologyRuleStore()).BuildGroupings([]DeviceGrouping{g})
	legacy := results[0].Instances[0]
	legacy.Alignments = nil
	legacy.Alignment = "pcieRoot"
	legacy.Rail = true
	client := fake.NewSimpleClientset()
	manager := NewDeviceClassManager(client, CoordinatorDriverName, NewTopologyRuleStore())
	ctx := context.Background()
	require.NoError(t, manager.SyncGroupingDeviceClasses(ctx, []GroupingResult{{Instances: []GroupingInstance{legacy}}}))
	fail := true
	client.PrependReactor("update", "deviceclasses", func(action ktesting.Action) (bool, runtime.Object, error) {
		if fail {
			return true, nil, fmt.Errorf("publication failed")
		}
		return false, nil, nil
	})
	require.Error(t, manager.SyncGroupingDeviceClasses(ctx, results))
	_, err := client.ResourceV1().DeviceClasses().Get(ctx, "local-rail0", metav1.GetOptions{})
	require.NoError(t, err)
	fail = false
	require.NoError(t, manager.SyncGroupingDeviceClasses(ctx, results))
	classes, err := client.ResourceV1().DeviceClasses().List(ctx, metav1.ListOptions{})
	require.NoError(t, err)
	require.Len(t, classes.Items, 1)
	require.Equal(t, "local", classes.Items[0].Name)
}

func TestPartitionConfigV3StrictNamedScopes(t *testing.T) {
	model, g := scopedTopology(t, "root-a", 0)
	inst := NewGroupingBuilder(model, NewTopologyRuleStore()).BuildGroupings([]DeviceGrouping{g})[0].Instances[0]
	config := NewDeviceClassManager(nil, CoordinatorDriverName, NewTopologyRuleStore()).buildGroupingAggregateConfig(inst)
	require.NoError(t, config.Validate())
	for _, refs := range [][]string{nil, {"gpu", "gpu"}, {"gpu.example"}, {"missing"}} {
		c := config
		c.Alignments = append([]AlignmentConfig(nil), config.Alignments...)
		c.Alignments[0].Requests = refs
		require.Error(t, c.Validate())
	}
	old := config
	old.APIVersion = PartitionConfigV1Alpha2
	require.Error(t, old.Validate())
}

func TestScopedCountedSearchAvoidsPermutations(t *testing.T) {
	for _, root := range []string{"root-a", "root-b"} {
		t.Run(root, func(t *testing.T) {
			model, g := scopedTopology(t, root, 0)
			g.Devices = g.Devices[:2]
			g.Devices[0].Count = 16
			g.Alignments = []GroupingAlignment{{Attribute: AttrPCIeRoot, Requests: []string{"gpu", "nic"}}}
			var devices []resourcev1.Device
			for i := 0; i < 16; i++ {
				devices = append(devices, resourcev1.Device{Name: fmt.Sprintf("gpu-%02d", i), Attributes: map[resourcev1.QualifiedName]resourcev1.DeviceAttribute{resourcev1.QualifiedName(AttrPCIeRoot): {StringValue: strPtr("root-a")}}})
			}
			model.UpdateFromResourceSlice(makeResourceSlice("gpu", "gpu.example", "node-1", "pool", devices))
			done := make(chan []GroupingResult, 1)
			go func() { done <- NewGroupingBuilder(model, NewTopologyRuleStore()).BuildGroupings([]DeviceGrouping{g}) }()
			select {
			case got := <-done:
				if root == "root-a" {
					require.Len(t, got, 1)
					require.Len(t, got[0].Instances[0].Devices, 17)
				} else {
					require.Empty(t, got)
				}
			case <-time.After(time.Second):
				t.Fatal("counted search enumerated equivalent permutations")
			}
		})
	}
}

func TestScopedUnsatisfiableConversionRemovesLegacyClasses(t *testing.T) {
	model, g := scopedTopology(t, "root-a", 0)
	builder := NewGroupingBuilder(model, NewTopologyRuleStore())
	legacy := builder.BuildGroupings([]DeviceGrouping{g})[0].Instances[0]
	legacy.Alignments = nil
	legacy.Alignment = "pcieRoot"
	legacy.Rail = true
	client := fake.NewSimpleClientset()
	manager := NewDeviceClassManager(client, CoordinatorDriverName, NewTopologyRuleStore())
	ctx := context.Background()
	require.NoError(t, manager.SyncGroupingDeviceClasses(ctx, []GroupingResult{{Instances: []GroupingInstance{legacy}}}))
	// A valid desired scoped contract with no native topology witness must not leave the old weaker contract available.
	model, _ = scopedTopology(t, "root-b", 0)
	results := NewGroupingBuilder(model, NewTopologyRuleStore()).BuildGroupings([]DeviceGrouping{g})
	require.Empty(t, results)
	require.NoError(t, manager.SyncGroupingDeviceClasses(ctx, results))
	classes, err := client.ResourceV1().DeviceClasses().List(ctx, metav1.ListOptions{})
	require.NoError(t, err)
	require.Empty(t, classes.Items)
}
