/*
Copyright © contributors to CloudNativePG, established as
CloudNativePG a Series of LF Projects, LLC.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.

SPDX-License-Identifier: Apache-2.0
*/

// Package storage provides kubectl plugin commands for storage management.
package storage

import (
	"github.com/spf13/cobra"

	"github.com/cloudnative-pg/cloudnative-pg/internal/cmd/plugin"
)

// NewCmd creates the new "storage" subcommand
func NewCmd() *cobra.Command {
	storageCmd := &cobra.Command{
		Use:     "storage",
		Short:   "Manage storage for PostgreSQL clusters",
		GroupID: plugin.GroupIDCluster,
	}

	storageCmd.AddCommand(newStatusCmd())

	return storageCmd
}

// newStatusCmd creates the new "storage status" subcommand
func newStatusCmd() *cobra.Command {
	statusCmd := &cobra.Command{
		Use:   "status CLUSTER",
		Short: "Get the storage status of a PostgreSQL cluster",
		Args:  plugin.RequiresArguments(1),
		ValidArgsFunction: func(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
			return plugin.CompleteClusters(cmd.Context(), args, toComplete), cobra.ShellCompDirectiveNoFileComp
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			clusterName := args[0]

			output, err := cmd.Flags().GetString("output")
			if err != nil {
				return err
			}

			return Status(ctx, clusterName, plugin.OutputFormat(output))
		},
	}

	statusCmd.Flags().StringP(
		"output", "o", "text", "Output format. One of text|json")

	return statusCmd
}
