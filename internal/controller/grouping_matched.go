package controller

import (
	"fmt"
	"sort"
)

// findMatchedInstances finds complete bundles on one physical counter set.
// shortcut: one instance per counter set can undercount small repeatable bundles; expand when the catalog count must represent multiple bundles per GPU.
func (b *GroupingBuilder) findMatchedInstances(nodeName string, devices []TopologyDevice, grouping DeviceGrouping, alignment string) []GroupingInstance {
	byAlignment := b.groupByAlignment(devices, alignment)
	keys := make([]string, 0, len(byAlignment))
	for key := range byAlignment {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var result []GroupingInstance
	for _, key := range keys {
		groups := make(map[string][]TopologyDevice)
		for _, dev := range byAlignment[key] {
			value, set := deviceAttributeValueString(dev, grouping.MatchAttribute), counterGroupID(dev)
			if value == "" || set == "" || len(dev.CounterCapacity) == 0 {
				continue
			}
			groups[value+"|"+set] = append(groups[value+"|"+set], dev)
		}
		groupKeys := make([]string, 0, len(groups))
		for group := range groups {
			groupKeys = append(groupKeys, group)
		}
		sort.Strings(groupKeys)
		for _, group := range groupKeys {
			candidates := groups[group]
			sort.Slice(candidates, func(i, j int) bool { return deviceKey(candidates[i]) < deviceKey(candidates[j]) })
			picked, ok := b.selectCounterBundle(candidates, grouping.Devices)
			if !ok {
				continue
			}
			inst := b.buildInstance(fmt.Sprintf("%s-%s-%s-%d", nodeName, grouping.Name, sanitizeForName(alignment), len(result)), nodeName, grouping, alignment, picked)
			inst.InstanceIndex = len(result)
			result = append(result, inst)
		}
	}
	return result
}

func (b *GroupingBuilder) selectCounterBundle(candidates []TopologyDevice, entries []GroupingDevice) ([]TopologyDevice, bool) {
	if len(candidates) == 0 {
		return nil, false
	}
	needed := 0
	for _, entry := range entries {
		if entry.Count < 1 || entry.Count > len(candidates)-needed {
			return nil, false
		}
		needed += entry.Count
	}
	slots := make([]GroupingDevice, 0, needed)
	for _, entry := range entries {
		for i := 0; i < entry.Count; i++ {
			slots = append(slots, entry)
		}
	}
	capacity := candidates[0].CounterCapacity
	var picked []TopologyDevice
	usedDevices := make(map[string]bool)
	usedCounters := make(map[string]int64)
	var search func(int) bool
	search = func(index int) bool {
		if index == len(slots) {
			return true
		}
		entry := slots[index]
		for _, dev := range candidates {
			if usedDevices[deviceKey(dev)] || b.rules.GetDeviceClassForDevice(dev) != entry.Class || (entry.Profile != "" && deviceProfile(dev) != entry.Profile) {
				continue
			}
			var delta = make(map[string]int64)
			fits := true
			for _, cc := range dev.ConsumesCounters {
				limits, ok := capacity[cc.CounterSet]
				if !ok {
					fits = false
					break
				}
				for name, counter := range cc.Counters {
					value := counter.Value.Value()
					limit, ok := limits[name]
					key := cc.CounterSet + "/" + name
					if !ok || usedCounters[key]+delta[key]+value > limit {
						fits = false
						break
					}
					delta[key] += value
				}
				if !fits {
					break
				}
			}
			if !fits {
				continue
			}
			usedDevices[deviceKey(dev)] = true
			picked = append(picked, dev)
			for name, value := range delta {
				usedCounters[name] += value
			}
			if search(index + 1) {
				return true
			}
			for name, value := range delta {
				usedCounters[name] -= value
			}
			picked = picked[:len(picked)-1]
			delete(usedDevices, deviceKey(dev))
		}
		return false
	}
	if !search(0) {
		return nil, false
	}
	return picked, true
}
