package main

import (
	"context"
	"strings"

	"github.com/spf13/cobra"
	"k8s.io/client-go/kubernetes"
)

func completeManagedNames(ctx context.Context, client kubernetes.Interface, prefix string) ([]string, error) {
	entries, err := loadEntries(ctx, client)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name, prefix) {
			names = append(names, entry.Name)
		}
	}
	return names, nil
}

func managedNameCompletion(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	if len(args) > 0 {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	client, err := newClient()
	if err != nil {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	names, err := completeManagedNames(cmd.Context(), client, toComplete)
	if err != nil {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	return names, cobra.ShellCompDirectiveNoFileComp
}
