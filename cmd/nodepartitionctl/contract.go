package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"

	resourcev1 "k8s.io/api/resource/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/rh-ecosystem-edge/k8s-dra-topology-coordinator/internal/controller"
)

// Contract is the safe, normalized request contract for one managed
// DeviceClass. It intentionally excludes CEL selectors and opaque parameters.
type Contract struct {
	Name       string              `json:"name"`
	Kind       string              `json:"kind"`
	Mode       string              `json:"mode,omitempty"`
	Alignment  string              `json:"alignment,omitempty"`
	Requests   []ContractRequest   `json:"requests"`
	Alignments []ContractAlignment `json:"alignments,omitempty"`
}

type ContractRequest struct {
	DeviceClass   string            `json:"deviceClass"`
	SourceDriver  string            `json:"sourceDriver,omitempty"`
	Count         int               `json:"count"`
	Capacity      map[string]string `json:"capacity,omitempty"`
	Passthrough   bool              `json:"passthrough,omitempty"`
	PartitionMode string            `json:"partitionMode,omitempty"`
}

type ContractAlignment struct {
	Attribute   string   `json:"attribute"`
	Requests    []string `json:"requests"`
	Enforcement string   `json:"enforcement"`
}

func runExplain(ctx context.Context, client kubernetes.Interface, w io.Writer, name, format string) error {
	if err := checkFormat(format); err != nil {
		return err
	}
	classes, err := client.ResourceV1().DeviceClasses().List(ctx, metav1.ListOptions{
		LabelSelector: controller.LabelManaged + "=true",
	})
	if err != nil {
		return fmt.Errorf("failed to list DeviceClasses: %w", err)
	}
	for _, dc := range classes.Items {
		if dc.Name != name {
			continue
		}
		contract, err := buildContract(dc)
		if err != nil {
			return err
		}
		if format == "json" {
			return writeJSON(w, contract)
		}
		return writeContractTable(w, contract)
	}
	return fmt.Errorf("no managed DeviceClass %q found", name)
}

func buildContract(dc resourcev1.DeviceClass) (Contract, error) {
	config, err := partitionConfig(dc)
	if err != nil {
		return Contract{}, err
	}

	kind := kindGrouping
	if _, ok := dc.Labels[controller.LabelPartitionType]; ok {
		kind = kindPartition
	} else if dc.Labels[controller.LabelGrouping] != "" {
		kind = groupingKind(dc)
	}

	mode := string(config.Mode)
	if mode == "" {
		mode = dc.Labels[controller.LabelMode]
	}
	if mode == "" {
		mode = string(controller.OfferingModeContainer)
	}
	requests := make([]ContractRequest, 0, len(config.SubResources))
	for _, sr := range config.SubResources {
		requests = append(requests, ContractRequest{
			DeviceClass:   sr.DeviceClass,
			SourceDriver:  sr.SourceDriver,
			Count:         sr.Count,
			Capacity:      copyStringMap(sr.Capacity),
			Passthrough:   sr.Passthrough,
			PartitionMode: sr.PartitionMode,
		})
	}
	alignments := make([]ContractAlignment, 0, len(config.Alignments))
	for _, alignment := range config.Alignments {
		alignments = append(alignments, ContractAlignment{
			Attribute:   alignment.Attribute,
			Requests:    append([]string(nil), alignment.Requests...),
			Enforcement: string(alignment.Enforcement),
		})
	}

	return Contract{
		Name:       dc.Name,
		Kind:       kind,
		Mode:       mode,
		Alignment:  dc.Labels[controller.LabelAlignment],
		Requests:   requests,
		Alignments: alignments,
	}, nil
}

func partitionConfig(dc resourcev1.DeviceClass) (controller.PartitionConfig, error) {
	for _, cfg := range dc.Spec.Config {
		if cfg.Opaque == nil || len(cfg.Opaque.Parameters.Raw) == 0 {
			continue
		}
		var partition controller.PartitionConfig
		if err := json.Unmarshal(cfg.Opaque.Parameters.Raw, &partition); err != nil {
			return controller.PartitionConfig{}, fmt.Errorf("managed DeviceClass %q has invalid PartitionConfig: %w", dc.Name, err)
		}
		if partition.Kind != "PartitionConfig" {
			continue
		}
		if partition.APIVersion != "" && partition.APIVersion != controller.PartitionConfigAPIVersion {
			return controller.PartitionConfig{}, fmt.Errorf("managed DeviceClass %q has unsupported PartitionConfig apiVersion %q", dc.Name, partition.APIVersion)
		}
		if err := partition.Validate(); err != nil {
			return controller.PartitionConfig{}, fmt.Errorf("managed DeviceClass %q has invalid PartitionConfig: %w", dc.Name, err)
		}
		return partition, nil
	}
	return controller.PartitionConfig{}, fmt.Errorf("managed DeviceClass %q has no PartitionConfig", dc.Name)
}

func copyStringMap(values map[string]string) map[string]string {
	if len(values) == 0 {
		return nil
	}
	copy := make(map[string]string, len(values))
	for key, value := range values {
		copy[key] = value
	}
	return copy
}

func writeContractTable(w io.Writer, contract Contract) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	for _, row := range [][]string{
		{"Name:", contract.Name},
		{"Kind:", contract.Kind},
		{"Offering mode:", dash(contract.Mode)},
		{"Topology alignment:", emptyValue(contract.Alignment)},
	} {
		if _, err := fmt.Fprintln(tw, strings.Join(row, "\t")); err != nil {
			return err
		}
	}
	if _, err := fmt.Fprintln(tw, "\nRequests:"); err != nil {
		return err
	}
	for _, request := range contract.Requests {
		capacity := formatCapacity(request.Capacity)
		passthrough := "no"
		if request.Passthrough {
			passthrough = "yes"
		}
		rows := [][]string{
			{"  Requested class:", request.DeviceClass},
			{"    Source driver:", dash(request.SourceDriver)},
			{"    Count:", strconv.Itoa(request.Count)},
			{"    Capacity request:", emptyValue(capacity)},
			{"    VM passthrough:", passthrough},
			{"    GPU partition profile:", emptyValue(request.PartitionMode)},
		}
		for _, row := range rows {
			if _, err := fmt.Fprintln(tw, strings.Join(row, "\t")); err != nil {
				return err
			}
		}
	}
	if len(contract.Alignments) > 0 {
		if _, err := fmt.Fprintln(tw, "\nAlignments:"); err != nil {
			return err
		}
		for _, alignment := range contract.Alignments {
			requests := strings.Join(alignment.Requests, ", ")
			if _, err := fmt.Fprintf(tw, "  %s\t%s\t%s\n", alignment.Attribute, alignment.Enforcement, requests); err != nil {
				return err
			}
		}
	}
	return tw.Flush()
}

func emptyValue(value string) string {
	if value == "" || value == "-" {
		return "none"
	}
	return value
}

func formatCapacity(capacity map[string]string) string {
	if len(capacity) == 0 {
		return "-"
	}
	keys := make([]string, 0, len(capacity))
	for key := range capacity {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		parts = append(parts, key+"="+capacity[key])
	}
	return strings.Join(parts, ", ")
}
