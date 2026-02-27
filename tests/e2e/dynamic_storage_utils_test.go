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

package e2e

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	apiv1 "github.com/cloudnative-pg/cloudnative-pg/api/v1"
	"github.com/cloudnative-pg/cloudnative-pg/pkg/utils"
)

// stiffenSpecForReliability configures the cluster spec with strict anti-affinity
// and topology spread constraints to ensure deterministic scheduling during E2E tests.
func stiffenSpecForReliability(cluster *apiv1.Cluster) {
	// Use required anti-affinity to ensure pods never land on the same node.
	// This makes disruption tests like node drains and failovers deterministic.
	cluster.Spec.Affinity.EnablePodAntiAffinity = ptr.To(true)
	cluster.Spec.Affinity.PodAntiAffinityType = apiv1.PodAntiAffinityTypeRequired

	// Add topology spread constraints to spread across hostnames.
	// This helps balance the CSI load across nodes.
	cluster.Spec.TopologySpreadConstraints = []corev1.TopologySpreadConstraint{
		{
			MaxSkew:           1,
			TopologyKey:       "kubernetes.io/hostname",
			WhenUnsatisfiable: corev1.DoNotSchedule,
			LabelSelector: &metav1.LabelSelector{
				MatchLabels: map[string]string{
					utils.ClusterLabelName: cluster.Name,
				},
			},
		},
	}
}
