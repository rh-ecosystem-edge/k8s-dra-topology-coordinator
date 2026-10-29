package main

import (
	"context"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"k8s.io/client-go/kubernetes"
)

var completionTimeout = 500 * time.Millisecond

func completeManagedNames(ctx context.Context, client kubernetes.Interface, prefix string) ([]string, error) {
	classes, err := loadManagedClasses(ctx, client)
	if err != nil {
		return nil, err
	}

	namesByName := make(map[string]struct{})
	for _, entry := range buildEntries(classes) {
		if strings.HasPrefix(entry.Name, prefix) {
			namesByName[entry.Name] = struct{}{}
		}
		for _, variant := range entry.Variants {
			if strings.HasPrefix(variant, prefix) {
				namesByName[variant] = struct{}{}
			}
		}
	}
	names := make([]string, 0, len(namesByName))
	for name := range namesByName {
		names = append(names, name)
	}
	sort.Strings(names)
	return names, nil
}

func completeManagedNamesWithTimeout(ctx context.Context, client kubernetes.Interface, prefix string) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, completionTimeout)
	defer cancel()
	return completeManagedNames(ctx, client, prefix)
}

func managedNameCompletion(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	if len(args) > 0 {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	client, err := newClient()
	if err != nil {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	names, err := completeManagedNamesWithTimeout(cmd.Context(), client, toComplete)
	if err != nil {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	return names, cobra.ShellCompDirectiveNoFileComp
}
