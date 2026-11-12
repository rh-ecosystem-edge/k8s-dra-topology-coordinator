package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"os"
	"strings"

	"github.com/spf13/cobra"
	resourcev1 "k8s.io/api/resource/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/kubernetes"
)

const previewGenerateName = "dratopoctl-preview-"

func newClaimsCmd() *cobra.Command {
	claims := &cobra.Command{Use: "claims", Short: "Preview ResourceClaim admission"}
	var file, offering, namespace, format string
	var count int64
	expand := &cobra.Command{
		Use:   "expand (-f FILE | --offering NAME)",
		Short: "Preview the ResourceClaim produced by webhook admission",
		Long:  "Submit a ResourceClaim, ResourceClaimTemplate, or curated offering as a server-side dry run and print the admitted ResourceClaim. No claim is saved. Offering count is the number of complete offering bundles. For templates and offerings, the preview claim has a synthetic name; this does not preview Pod or VMI request rewrites or allocation.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) (runErr error) {
			if err := validateExpandInput(cmd.Flags().Changed("file"), file, cmd.Flags().Changed("offering"), offering, cmd.Flags().Changed("count"), count); err != nil {
				return err
			}
			if format != "json" {
				return fmt.Errorf("invalid --output %q: claims expand supports json only", format)
			}
			config := kubeClientConfig()
			contextNamespace, _, err := config.Namespace()
			if err != nil {
				return fmt.Errorf("failed to load kubeconfig namespace: %w", err)
			}
			client, err := clientFromConfig(config)
			if err != nil {
				return err
			}
			if cmd.Flags().Changed("offering") {
				err = runExpandOffering(cmd.Context(), client, cmd.OutOrStdout(), offering, count, namespace, contextNamespace)
			} else {
				var input = cmd.InOrStdin()
				if file != "-" {
					f, openErr := os.Open(file)
					if openErr != nil {
						return fmt.Errorf("open %q: %w", file, openErr)
					}
					defer func() {
						if closeErr := f.Close(); closeErr != nil && runErr == nil {
							runErr = fmt.Errorf("close %q: %w", file, closeErr)
						}
					}()
					input = f
				}
				err = runExpand(cmd.Context(), client, cmd.OutOrStdout(), input, namespace, contextNamespace)
			}
			if err != nil {
				return err
			}
			_, err = fmt.Fprintln(cmd.ErrOrStderr(), "Preview only: server-side dry run; no ResourceClaim was saved.")
			return err
		},
	}
	expand.Flags().StringVarP(&file, "file", "f", "", "ResourceClaim or ResourceClaimTemplate YAML/JSON file (- for stdin)")
	expand.Flags().StringVar(&offering, "offering", "", "Curated offering name to preview")
	expand.Flags().Int64Var(&count, "count", 1, "Number of whole offering bundles to request (with --offering)")
	expand.Flags().StringVarP(&namespace, "namespace", "n", "", "Namespace when the input does not specify one")
	expand.Flags().StringVarP(&format, "output", "o", "json", "Output format: json")
	claims.AddCommand(expand)
	return claims
}

func runExpand(ctx context.Context, client kubernetes.Interface, w io.Writer, input io.Reader, namespace, contextNamespace string) error {
	claim, err := previewClaim(input)
	if err != nil {
		return err
	}
	return runExpandClaim(ctx, client, w, claim, namespace, contextNamespace)
}

func runExpandOffering(ctx context.Context, client kubernetes.Interface, w io.Writer, name string, count int64, namespace, contextNamespace string) error {
	claim, err := offeringPreviewClaim(name, count)
	if err != nil {
		return err
	}
	entries, err := loadEntries(ctx, client)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.Name != name {
			continue
		}
		if entry.Kind != kindOffering {
			return fmt.Errorf("managed DeviceClass %q is not a curated offering", name)
		}
		return runExpandClaim(ctx, client, w, claim, namespace, contextNamespace)
	}
	return fmt.Errorf("no curated offering %q found", name)
}

func offeringPreviewClaim(name string, count int64) (*resourcev1.ResourceClaim, error) {
	if count < 1 {
		return nil, fmt.Errorf("count must be positive")
	}
	return &resourcev1.ResourceClaim{
		TypeMeta:   metav1.TypeMeta{APIVersion: "resource.k8s.io/v1", Kind: "ResourceClaim"},
		ObjectMeta: metav1.ObjectMeta{GenerateName: previewGenerateName},
		Spec: resourcev1.ResourceClaimSpec{Devices: resourcev1.DeviceClaim{Requests: []resourcev1.DeviceRequest{{
			Name:    "offering",
			Exactly: &resourcev1.ExactDeviceRequest{DeviceClassName: name, Count: count},
		}}}},
	}, nil
}

func validateExpandInput(fileSet bool, file string, offeringSet bool, offering string, countSet bool, count int64) error {
	if fileSet == offeringSet {
		return fmt.Errorf("specify exactly one of --file or --offering; they are mutually exclusive")
	}
	if fileSet {
		if file == "" {
			return fmt.Errorf("file path must not be empty")
		}
		if countSet {
			return fmt.Errorf("--count requires --offering")
		}
		return nil
	}
	if strings.TrimSpace(offering) == "" {
		return fmt.Errorf("offering name must not be empty")
	}
	if count < 1 {
		return fmt.Errorf("count must be positive")
	}
	return nil
}

func runExpandClaim(ctx context.Context, client kubernetes.Interface, w io.Writer, claim *resourcev1.ResourceClaim, namespace, contextNamespace string) error {
	if claim.Namespace != "" && namespace != "" && claim.Namespace != namespace {
		return fmt.Errorf("input namespace %q conflicts with --namespace %q", claim.Namespace, namespace)
	}
	if claim.Namespace == "" {
		claim.Namespace = namespace
	}
	if claim.Namespace == "" {
		claim.Namespace = contextNamespace
	}
	if claim.Namespace == "" {
		claim.Namespace = "default"
	}
	admitted, err := client.ResourceV1().ResourceClaims(claim.Namespace).Create(ctx, claim, metav1.CreateOptions{DryRun: []string{metav1.DryRunAll}})
	if err != nil {
		name := claim.Name
		if name == "" {
			name = claim.GenerateName + "*"
		}
		return fmt.Errorf("preview ResourceClaim %s/%s: %w", claim.Namespace, name, err)
	}
	admitted.APIVersion = "resource.k8s.io/v1"
	admitted.Kind = "ResourceClaim"
	return writeJSON(w, admitted)
}

func previewClaim(input io.Reader) (*resourcev1.ResourceClaim, error) {
	decoder := yaml.NewYAMLOrJSONDecoder(input, 4096)
	var selected *resourcev1.ResourceClaim
	for {
		var raw json.RawMessage
		err := decoder.Decode(&raw)
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("decode input: %w", err)
		}
		if len(bytes.TrimSpace(raw)) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			continue
		}
		var kind metav1.TypeMeta
		if err := json.Unmarshal(raw, &kind); err != nil {
			return nil, fmt.Errorf("decode object kind: %w", err)
		}
		if kind.Kind != "ResourceClaim" && kind.Kind != "ResourceClaimTemplate" {
			continue
		}
		if kind.APIVersion != "resource.k8s.io/v1" {
			return nil, fmt.Errorf("unsupported %s apiVersion %q: expected resource.k8s.io/v1", kind.Kind, kind.APIVersion)
		}
		if selected != nil {
			return nil, fmt.Errorf("input contains more than one ResourceClaim or ResourceClaimTemplate")
		}
		if kind.Kind == "ResourceClaim" {
			selected = &resourcev1.ResourceClaim{}
			if err := json.Unmarshal(raw, selected); err != nil {
				return nil, fmt.Errorf("decode ResourceClaim: %w", err)
			}
			if selected.Name == "" && selected.GenerateName == "" {
				selected.GenerateName = previewGenerateName
			}
			continue
		}
		var template resourcev1.ResourceClaimTemplate
		if err := json.Unmarshal(raw, &template); err != nil {
			return nil, fmt.Errorf("decode ResourceClaimTemplate: %w", err)
		}
		selected = &resourcev1.ResourceClaim{
			TypeMeta: metav1.TypeMeta{APIVersion: "resource.k8s.io/v1", Kind: "ResourceClaim"},
			ObjectMeta: metav1.ObjectMeta{
				Namespace:    template.Namespace,
				GenerateName: previewGenerateName,
				Labels:       maps.Clone(template.Spec.Labels),
				Annotations:  maps.Clone(template.Spec.Annotations),
			},
			Spec: *template.Spec.Spec.DeepCopy(),
		}
	}
	if selected == nil {
		return nil, fmt.Errorf("input contains no ResourceClaim or ResourceClaimTemplate")
	}
	return selected, nil
}
