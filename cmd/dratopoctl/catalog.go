package main

import (
	"sort"
	"strconv"
	"strings"

	resourcev1 "k8s.io/api/resource/v1"

	"github.com/rh-ecosystem-edge/k8s-dra-topology-coordinator/internal/controller"
)

// Entry kinds, derived from the labels the controller publishes.
const (
	kindOffering     = "offering"      // aggregate class of a grouping labeled offering=true
	kindGrouping     = "grouping"      // aggregate class of any other ConfigMap grouping
	kindAutoDetected = "auto-detected" // aggregate class of a coordinator-derived grouping
	kindPartition    = "partition"     // fixed-hierarchy partition class
)

// Entry is the user-facing view of one managed DeviceClass. It carries
// discovery metadata only: never the class selectors or opaque configuration.
type Entry struct {
	Name          string   `json:"name"`
	Kind          string   `json:"kind"`
	DisplayName   string   `json:"displayName,omitempty"`
	Description   string   `json:"description,omitempty"`
	IntentLevel   string   `json:"intentLevel,omitempty"`
	Visibility    string   `json:"visibility,omitempty"`
	Categories    []string `json:"categories,omitempty"`
	WorkloadTypes []string `json:"workloadTypes,omitempty"`
	Mode          string   `json:"mode,omitempty"`
	Alignment     string   `json:"alignment,omitempty"`
	Count         int      `json:"count"`
	Variants      []string `json:"variants,omitempty"`
}

// buildEntries converts coordinator-managed DeviceClasses into catalog
// entries, sorted by name. Per-instance grouping classes are folded into the
// Variants of their aggregate rather than listed on their own.
func buildEntries(classes []resourcev1.DeviceClass) []Entry {
	type aggregate struct {
		entry    Entry
		grouping string
	}
	var aggregates []aggregate
	var entries []Entry
	variants := map[string][]string{}
	for _, dc := range classes {
		if dc.Labels[controller.LabelManaged] != "true" {
			continue
		}
		if _, ok := dc.Labels[controller.LabelPartitionType]; ok {
			entries = append(entries, newEntry(dc, kindPartition))
			continue
		}
		grouping, ok := dc.Labels[controller.LabelGrouping]
		if !ok {
			continue
		}
		if dc.Labels[controller.LabelAggregate] == "true" {
			aggregates = append(aggregates, aggregate{newEntry(dc, groupingKind(dc)), grouping})
		} else {
			variants[grouping] = append(variants[grouping], dc.Name)
		}
	}
	for _, a := range aggregates {
		sort.Strings(variants[a.grouping])
		a.entry.Variants = variants[a.grouping]
		entries = append(entries, a.entry)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name < entries[j].Name })
	return entries
}

func groupingKind(dc resourcev1.DeviceClass) string {
	switch {
	case dc.Labels[controller.LabelOffering] == "true":
		return kindOffering
	case dc.Labels[controller.LabelSource] == controller.SourceAutoDetected:
		return kindAutoDetected
	default:
		return kindGrouping
	}
}

func newEntry(dc resourcev1.DeviceClass, kind string) Entry {
	count := 1
	if n, err := strconv.Atoi(dc.Labels[controller.LabelCount]); err == nil && n > 0 {
		count = n
	}
	return Entry{
		Name:          dc.Name,
		Kind:          kind,
		DisplayName:   dc.Annotations[controller.AnnotationDisplayName],
		Description:   dc.Annotations[controller.AnnotationDescription],
		IntentLevel:   dc.Labels[controller.LabelIntentLevel],
		Visibility:    dc.Labels[controller.LabelVisibility],
		Categories:    splitList(dc.Annotations[controller.AnnotationCategories]),
		WorkloadTypes: splitList(dc.Annotations[controller.AnnotationWorkloadTypes]),
		Mode:          dc.Labels[controller.LabelMode],
		Alignment:     topologyLevel(dc),
		Count:         count,
	}
}

func splitList(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(s, ",")
}

// filterEntries keeps curated offerings unless all is set.
func filterEntries(entries []Entry, all bool) []Entry {
	if all {
		return entries
	}
	var out []Entry
	for _, e := range entries {
		if e.Kind == kindOffering {
			out = append(out, e)
		}
	}
	return out
}
