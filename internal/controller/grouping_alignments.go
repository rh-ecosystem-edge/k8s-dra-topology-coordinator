package controller

import (
	"fmt"
	"sort"
	"strings"

	resourcev1 "k8s.io/api/resource/v1"
)

// GroupingAlignment requires the named entries to share a published attribute.
type GroupingAlignment struct {
	Attribute string   `json:"attribute"`
	Requests  []string `json:"requests"`
}

func validateGroupingAlignments(alignments []GroupingAlignment, names map[string]struct{}) error {
	if len(alignments) == 0 || len(alignments) > resourcev1.DeviceConstraintsMaxSize {
		return fmt.Errorf("alignments must contain 1 to %d entries", resourcev1.DeviceConstraintsMaxSize)
	}
	for i, a := range alignments {
		if a.Attribute != AttrPCIeRoot && a.Attribute != AttrNUMANode {
			return fmt.Errorf("alignments[%d] has unsupported attribute %q", i, a.Attribute)
		}
		if len(a.Requests) == 0 {
			return fmt.Errorf("alignments[%d] requests must not be empty", i)
		}
		seen := map[string]bool{}
		for _, name := range a.Requests {
			if _, ok := names[name]; !ok || name == "" {
				return fmt.Errorf("alignments[%d] references unknown name %q", i, name)
			}
			if seen[name] {
				return fmt.Errorf("alignments[%d] repeats name %q", i, name)
			}
			seen[name] = true
		}
	}
	return nil
}

func cloneGroupingAlignments(in []GroupingAlignment) []GroupingAlignment {
	if in == nil {
		return nil
	}
	out := make([]GroupingAlignment, len(in))
	for i, a := range in {
		out[i] = a
		out[i].Requests = append([]string(nil), a.Requests...)
	}
	return out
}

func canonicalGroupingAlignments(in []GroupingAlignment) []GroupingAlignment {
	out := cloneGroupingAlignments(in)
	for i := range out {
		sort.Strings(out[i].Requests)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Attribute != out[j].Attribute {
			return out[i].Attribute < out[j].Attribute
		}
		return strings.Join(out[i].Requests, ",") < strings.Join(out[j].Requests, ",")
	})
	return out
}
