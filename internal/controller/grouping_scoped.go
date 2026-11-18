package controller

import (
	"fmt"
	"sort"

	"k8s.io/apimachinery/pkg/api/resource"
)

type scopedCounterKey struct{ driver, pool, set, name string }

// findScopedInstances keeps alternatives until all required alignments have been checked.
func (b *GroupingBuilder) findScopedInstances(node string, devices []TopologyDevice, grouping DeviceGrouping) []GroupingInstance {
	sorted := append([]TopologyDevice(nil), devices...)
	sort.Slice(sorted, func(i, j int) bool { return deviceKey(sorted[i]) < deviceKey(sorted[j]) })
	pools := make(map[string][]TopologyDevice)
	memberships := make(map[string][]int)
	for i, a := range grouping.Alignments {
		for _, name := range a.Requests {
			memberships[name] = append(memberships[name], i)
		}
	}
	var slots []GroupingDevice
	for _, entry := range grouping.Devices {
		for _, dev := range sorted {
			if b.rules.GetDeviceClassForDevice(dev) == entry.Class && (entry.Profile == "" || deviceProfile(dev) == entry.Profile) {
				pools[entry.Name] = append(pools[entry.Name], dev)
			}
		}
		if entry.Count < 1 || entry.Count > len(pools[entry.Name]) || entry.Count > len(devices)-len(slots) {
			return nil
		}
		for i := 0; i < entry.Count; i++ {
			slots = append(slots, entry)
		}
	}
	taken := map[string]bool{}
	used := map[scopedCounterKey]resource.Quantity{}
	var results []GroupingInstance
	for {
		values := map[int]string{}
		var picked []TopologyDevice
		var search func(int) bool
		search = func(index int) bool {
			if index == len(slots) {
				return true
			}
			entry := slots[index]
			for _, dev := range pools[entry.Name] {
				key := deviceKey(dev)
				if taken[key] {
					continue
				}
				valid := true
				candidateValues := map[int]string{}
				for _, ai := range memberships[entry.Name] {
					a := grouping.Alignments[ai]
					v, ok := dev.PublishedTopology[a.Attribute]
					if !ok || (a.Attribute == AttrNUMANode && v.IntValue == nil) || (a.Attribute == AttrPCIeRoot && v.StringValue == nil) {
						valid = false
						break
					}
					value := v.String()
					if current, exists := values[ai]; exists && current != value {
						valid = false
						break
					}
					candidateValues[ai] = value
				}
				if !valid {
					continue
				}
				delta := map[scopedCounterKey]resource.Quantity{}
				for _, cc := range dev.ConsumesCounters {
					for name, counter := range cc.Counters {
						ck := scopedCounterKey{dev.DriverName, dev.PoolName, cc.CounterSet, name}
						limit, ok := dev.PublishedCounters[cc.CounterSet][name]
						if !ok || limit.Sign() < 0 || counter.Value.Sign() < 0 {
							valid = false
							break
						}
						q := delta[ck]
						q.Add(counter.Value)
						delta[ck] = q
						total := used[ck].DeepCopy()
						total.Add(q)
						if total.Cmp(limit) > 0 {
							valid = false
							break
						}
					}
					if !valid {
						break
					}
				}
				if !valid {
					continue
				}
				var added []int
				for ai, value := range candidateValues {
					if _, exists := values[ai]; !exists {
						values[ai] = value
						added = append(added, ai)
					}
				}
				for ck, q := range delta {
					total := used[ck]
					total.Add(q)
					used[ck] = total
				}
				taken[key] = true
				picked = append(picked, dev)
				if search(index + 1) {
					return true
				}
				picked = picked[:len(picked)-1]
				delete(taken, key)
				for _, ai := range added {
					delete(values, ai)
				}
				for ck, q := range delta {
					total := used[ck]
					total.Sub(q)
					used[ck] = total
				}
			}
			return false
		}
		if !search(0) {
			return results
		}
		inst := b.buildInstance(fmt.Sprintf("%s-%s-mixed-%d", node, grouping.Name, len(results)), node, grouping, "mixed", picked)
		inst.InstanceIndex = len(results)
		results = append(results, inst)
	}
}
