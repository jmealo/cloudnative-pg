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

// Package autoresize implements automatic PVC resizing for CloudNativePG clusters.
// It monitors disk usage and triggers PVC expansion when configured thresholds
// are reached, respecting rate limits and WAL safety policies.
package autoresize

import (
	"fmt"
	"strconv"
	"strings"

	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/util/intstr"

	apiv1 "github.com/cloudnative-pg/cloudnative-pg/api/v1"
)

const (
	defaultStepPercent = "20%"
	defaultMinStep     = "2Gi"
	defaultMaxStep     = "500Gi"
)

// isPercentageStep checks if the step is specified as a percentage.
func isPercentageStep(step intstr.IntOrString) bool {
	if step.Type != intstr.String {
		return false
	}
	return strings.HasSuffix(step.StrVal, "%")
}

// parsePercentage extracts the percentage value from a string like "20%".
func parsePercentage(step intstr.IntOrString) (int, error) {
	if step.Type != intstr.String {
		return 0, fmt.Errorf("step is not a string type")
	}

	strVal := strings.TrimSuffix(step.StrVal, "%")
	percent, err := strconv.Atoi(strVal)
	if err != nil {
		return 0, fmt.Errorf("failed to parse percentage from '%s': %w", step.StrVal, err)
	}

	if percent < 0 || percent > 100 {
		return 0, fmt.Errorf("percentage out of range: %d", percent)
	}

	return percent, nil
}

// CalculateNewSize computes the new PVC size based on the expansion policy and current size.
func CalculateNewSize(currentSize resource.Quantity, policy *apiv1.ExpansionPolicy) (resource.Quantity, error) {
	if policy == nil {
		return currentSize, fmt.Errorf("expansion policy is nil")
	}

	// Determine the step to use
	stepVal := policy.Step
	// Default step when zero value (either empty string or zero int)
	if (stepVal.Type == intstr.String && stepVal.StrVal == "") ||
		(stepVal.Type == intstr.Int && stepVal.IntVal == 0) {
		stepVal = intstr.FromString(defaultStepPercent)
	}

	var expansionStep resource.Quantity

	if isPercentageStep(stepVal) {
		// Handle percentage-based step
		percent, err := parsePercentage(stepVal)
		if err != nil {
			return currentSize, fmt.Errorf("invalid percentage step: %w", err)
		}

		// Calculate raw step: currentSize * (percent / 100)
		rawStep := resource.NewQuantity(currentSize.Value()*int64(percent)/100, currentSize.Format)

		// Parse min and max step constraints
		minStepQty := parseQuantityOrDefault(policy.MinStep, defaultMinStep)
		maxStepQty := parseQuantityOrDefault(policy.MaxStep, defaultMaxStep)

		// Clamp the step: max(minStep, min(rawStep, maxStep))
		switch {
		case rawStep.Cmp(*minStepQty) < 0:
			expansionStep = *minStepQty
		case rawStep.Cmp(*maxStepQty) > 0:
			expansionStep = *maxStepQty
		default:
			expansionStep = *rawStep
		}
	} else {
		// Absolute value step - parse as Quantity, ignore minStep/maxStep
		var err error
		expansionStep, err = resource.ParseQuantity(stepVal.StrVal)
		if err != nil {
			return currentSize, fmt.Errorf("failed to parse step as quantity: %w", err)
		}
	}

	// Calculate new size: currentSize + expansionStep
	newSize := resource.NewQuantity(currentSize.Value()+expansionStep.Value(), currentSize.Format)

	// Apply limit if specified
	if policy.Limit != "" {
		limit, err := resource.ParseQuantity(policy.Limit)
		if err != nil {
			return currentSize, fmt.Errorf("failed to parse limit: %w", err)
		}

		if limit.Value() > 0 && newSize.Cmp(limit) > 0 {
			newSize = &limit
		}
	}

	return *newSize, nil
}

// parseQuantityOrDefault attempts to parse a quantity string, returning a default if empty or invalid.
func parseQuantityOrDefault(qtyStr string, defaultStr string) *resource.Quantity {
	if qtyStr == "" {
		qty, _ := resource.ParseQuantity(defaultStr)
		return &qty
	}

	qty, err := resource.ParseQuantity(qtyStr)
	if err != nil {
		fallback, _ := resource.ParseQuantity(defaultStr)
		return &fallback
	}

	return &qty
}
