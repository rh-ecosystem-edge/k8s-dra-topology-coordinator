// Command nodepartitionctl is a read-only catalog client for the Node
// Partition Topology Coordinator. It lists the DeviceClasses the coordinator
// manages and the offerings built on them.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/rh-ecosystem-edge/k8s-dra-topology-coordinator/internal/controller"
)

var (
	kubeconfig  string
	kubeContext string
	output      string
	showAll     bool
)

func main() {
	if err := newRootCmd().Execute(); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}

func newRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:           "nodepartitionctl",
		Short:         "Discover Node Partition Topology Coordinator offerings",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.PersistentFlags().StringVar(&kubeconfig, "kubeconfig", "",
		"Path to kubeconfig file (default: $KUBECONFIG or ~/.kube/config)")
	root.PersistentFlags().StringVar(&kubeContext, "context", "", "Kubeconfig context to use")
	root.PersistentFlags().StringVarP(&output, "output", "o", "table", "Output format: table or json")

	offerings := &cobra.Command{Use: "offerings", Short: "List and inspect offerings"}
	list := &cobra.Command{
		Use:   "list",
		Short: "List offerings (curated offerings by default)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			client, err := newClient()
			if err != nil {
				return err
			}
			return runList(cmd.Context(), client, cmd.OutOrStdout(), showAll, output)
		},
	}
	list.Flags().BoolVar(&showAll, "all", false,
		"Include grouping, auto-detected, and partition classes, not only curated offerings")
	get := &cobra.Command{
		Use:   "get NAME",
		Short: "Show one offering or managed class",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := newClient()
			if err != nil {
				return err
			}
			return runGet(cmd.Context(), client, cmd.OutOrStdout(), args[0], output)
		},
	}
	offerings.AddCommand(list, get)
	root.AddCommand(offerings)
	return root
}

func newClient() (kubernetes.Interface, error) {
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	rules.ExplicitPath = kubeconfig
	cfg, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		rules, &clientcmd.ConfigOverrides{CurrentContext: kubeContext}).ClientConfig()
	if err != nil {
		return nil, fmt.Errorf("failed to load kubeconfig: %w", err)
	}
	client, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("failed to create Kubernetes client: %w", err)
	}
	return client, nil
}

func loadEntries(ctx context.Context, client kubernetes.Interface) ([]Entry, error) {
	classes, err := client.ResourceV1().DeviceClasses().List(ctx, metav1.ListOptions{
		LabelSelector: controller.LabelManaged + "=true",
	})
	if err != nil {
		return nil, fmt.Errorf("failed to list DeviceClasses: %w", err)
	}
	return buildEntries(classes.Items), nil
}

func runList(ctx context.Context, client kubernetes.Interface, w io.Writer, all bool, format string) error {
	if err := checkFormat(format); err != nil {
		return err
	}
	entries, err := loadEntries(ctx, client)
	if err != nil {
		return err
	}
	entries = filterEntries(entries, all)
	if format == "json" {
		if entries == nil {
			entries = []Entry{}
		}
		return writeJSON(w, entries)
	}
	if len(entries) == 0 {
		_, err := fmt.Fprintln(w, "No offerings found. Use --all to include non-curated classes.")
		return err
	}
	rows := [][]string{{"NAME", "KIND", "DISPLAY NAME", "INTENT", "VISIBILITY", "MODE", "ALIGNMENT", "COUNT", "VARIANTS"}}
	for _, e := range entries {
		rows = append(rows, []string{e.Name, e.Kind, dash(e.DisplayName), dash(e.IntentLevel), dash(e.Visibility),
			dash(e.Mode), dash(e.Alignment), strconv.Itoa(e.Count), strconv.Itoa(len(e.Variants))})
	}
	return writeTable(w, rows)
}

func runGet(ctx context.Context, client kubernetes.Interface, w io.Writer, name, format string) error {
	if err := checkFormat(format); err != nil {
		return err
	}
	entries, err := loadEntries(ctx, client)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e.Name != name {
			continue
		}
		if format == "json" {
			return writeJSON(w, e)
		}
		return writeTable(w, [][]string{
			{"Name:", e.Name},
			{"Kind:", e.Kind},
			{"Display name:", dash(e.DisplayName)},
			{"Description:", dash(e.Description)},
			{"Intent level:", dash(e.IntentLevel)},
			{"Visibility:", dash(e.Visibility)},
			{"Categories:", dash(strings.Join(e.Categories, ", "))},
			{"Workload types:", dash(strings.Join(e.WorkloadTypes, ", "))},
			{"Mode:", dash(e.Mode)},
			{"Alignment:", dash(e.Alignment)},
			{"Count:", strconv.Itoa(e.Count)},
			{"Variants:", dash(strings.Join(e.Variants, ", "))},
		})
	}
	return fmt.Errorf("no managed DeviceClass %q found", name)
}

func writeTable(w io.Writer, rows [][]string) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	for _, row := range rows {
		if _, err := fmt.Fprintln(tw, strings.Join(row, "\t")); err != nil {
			return err
		}
	}
	return tw.Flush()
}

func checkFormat(format string) error {
	if format != "table" && format != "json" {
		return fmt.Errorf("invalid --output %q: must be table or json", format)
	}
	return nil
}

func writeJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
