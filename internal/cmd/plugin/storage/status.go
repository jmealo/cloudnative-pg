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

package storage

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"github.com/cheynewallace/tabby"
	"github.com/logrusorgru/aurora/v4"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	apiv1 "github.com/cloudnative-pg/cloudnative-pg/api/v1"
	"github.com/cloudnative-pg/cloudnative-pg/internal/cmd/plugin"
	"github.com/cloudnative-pg/cloudnative-pg/pkg/reconciler/dynamicstorage"
	"github.com/cloudnative-pg/cloudnative-pg/pkg/utils"
)

// Info contains the storage information of the Cluster
type Info struct {
	// Cluster is the Cluster we are investigating
	Cluster *apiv1.Cluster `json:"cluster"`

	// PVCs is the list of PVCs for this cluster
	PVCs []corev1.PersistentVolumeClaim `json:"pvcs"`
}

// Status implements the "storage status" subcommand
func Status(
	ctx context.Context,
	clusterName string,
	format plugin.OutputFormat,
) error {
	var cluster apiv1.Cluster
	if err := plugin.Client.Get(ctx, client.ObjectKey{
		Namespace: plugin.Namespace,
		Name:      clusterName,
	}, &cluster); err != nil {
		return err
	}

	// Get PVCs for this cluster
	var pvcList corev1.PersistentVolumeClaimList
	if err := plugin.Client.List(ctx, &pvcList,
		client.InNamespace(plugin.Namespace),
		client.MatchingLabels{utils.ClusterLabelName: clusterName},
	); err != nil {
		return err
	}

	status := Info{
		Cluster: &cluster,
		PVCs:    pvcList.Items,
	}

	switch format {
	case plugin.OutputFormatJSON:
		return printJSON(status)
	default:
		return printText(status)
	}
}

func printJSON(status Info) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(status)
}

func printText(status Info) error {
	cluster := status.Cluster

	// Print header
	fmt.Printf("Cluster: %s\n", aurora.Bold(cluster.Name))

	// Check if dynamic sizing is enabled
	isDynamic := dynamicstorage.IsDynamicSizingEnabled(&cluster.Spec.StorageConfiguration)
	if isDynamic {
		fmt.Printf("Dynamic Sizing: %s\n\n", aurora.Green("Enabled"))
	} else {
		fmt.Printf("Dynamic Sizing: %s\n\n", aurora.Yellow("Disabled"))
		fmt.Printf("Size: %s\n", cluster.Spec.StorageConfiguration.Size)
		fmt.Println()
	}

	if isDynamic {
		printDynamicStorageConfig(cluster)
		printDynamicStatus(cluster)
	}

	printPVCTable(status.PVCs)

	return nil
}

func printDynamicStorageConfig(cluster *apiv1.Cluster) {
	cfg := &cluster.Spec.StorageConfiguration

	fmt.Println(aurora.Bold("Data Volume Configuration:"))
	fmt.Printf("  Request:        %s\n", cfg.Request)
	fmt.Printf("  Limit:          %s\n", cfg.Limit)
	fmt.Printf("  Target Buffer:  %d%%\n", dynamicstorage.GetTargetBuffer(cfg))

	if cfg.MaintenanceWindow != nil {
		fmt.Printf("  Maintenance:    %s (%s, %s)\n",
			cfg.MaintenanceWindow.Schedule,
			cfg.MaintenanceWindow.Duration,
			cfg.MaintenanceWindow.Timezone)
	}

	if cfg.EmergencyGrow != nil {
		fmt.Printf("  Emergency:      threshold=%d%%, minFree=%s\n",
			dynamicstorage.GetCriticalThreshold(cfg),
			cfg.EmergencyGrow.CriticalMinimumFree)
	}
	fmt.Println()
}

func printDynamicStatus(cluster *apiv1.Cluster) {
	if cluster.Status.StorageSizing == nil || cluster.Status.StorageSizing.Data == nil {
		fmt.Println(aurora.Yellow("No dynamic storage status available yet"))
		fmt.Println()
		return
	}

	status := cluster.Status.StorageSizing.Data

	fmt.Println(aurora.Bold("Data Volume Status:"))
	fmt.Printf("  Effective Size: %s\n", status.EffectiveSize)
	fmt.Printf("  Target Size:    %s\n", status.TargetSize)

	// Print state with color
	stateStr := status.State
	switch stateStr {
	case "Balanced":
		stateStr = aurora.Green(stateStr).String()
	case "Emergency":
		stateStr = aurora.Red(stateStr).String()
	case "PendingGrowth", "Resizing":
		stateStr = aurora.Yellow(stateStr).String()
	}
	fmt.Printf("  State:          %s\n", stateStr)

	// Print budget info
	if status.Budget != nil {
		fmt.Println()
		fmt.Println(aurora.Bold("Budget:"))
		cfg := &cluster.Spec.StorageConfiguration
		fmt.Printf("  Max Actions/Day:      %d\n", dynamicstorage.GetMaxActionsPerDay(cfg))
		fmt.Printf("  Used (24h):           %d\n", status.Budget.ActionsLast24h)
		fmt.Printf("  Available (Planned):  %d\n", status.Budget.AvailableForPlanned)
		fmt.Printf("  Available (Emergency): %d\n", status.Budget.AvailableForEmergency)
		fmt.Printf("  Resets At:            %s\n", status.Budget.BudgetResetsAt.Format("2006-01-02T15:04:05Z"))
	}

	// Print next maintenance window
	if status.NextMaintenanceWindow != nil {
		fmt.Printf("\nNext Maintenance Window: %s\n", status.NextMaintenanceWindow.Format("2006-01-02T15:04:05Z"))
	}

	// Print last action
	if status.LastAction != nil {
		fmt.Println()
		fmt.Println(aurora.Bold("Last Action:"))
		fmt.Printf("  Kind:      %s\n", status.LastAction.Kind)
		fmt.Printf("  From:      %s\n", status.LastAction.From)
		fmt.Printf("  To:        %s\n", status.LastAction.To)
		fmt.Printf("  Timestamp: %s\n", status.LastAction.Timestamp.Format("2006-01-02T15:04:05Z"))
		fmt.Printf("  Instance:  %s\n", status.LastAction.Instance)
		fmt.Printf("  Result:    %s\n", status.LastAction.Result)
	}

	fmt.Println()
}

func printPVCTable(pvcs []corev1.PersistentVolumeClaim) {
	if len(pvcs) == 0 {
		fmt.Println(aurora.Yellow("No PVCs found"))
		return
	}

	fmt.Println(aurora.Bold("PVCs:"))
	t := tabby.New()
	t.AddHeader("NAME", "ROLE", "SIZE", "STATUS")

	for _, pvc := range pvcs {
		role := pvc.Labels[utils.PvcRoleLabelName]
		size := pvc.Spec.Resources.Requests.Storage().String()
		phase := string(pvc.Status.Phase)

		t.AddLine(pvc.Name, role, size, phase)
	}

	t.Print()
}
