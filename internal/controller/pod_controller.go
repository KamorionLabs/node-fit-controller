/*
Copyright 2025 KamorionLabs.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

const (
	// Annotations for opt-in and configuration
	AnnotationEnabled  = "nodefit.io/enabled"
	AnnotationStrategy = "nodefit.io/strategy"
	AnnotationPercent  = "nodefit.io/percent"  // For percent strategy
	AnnotationBuffer   = "nodefit.io/buffer"   // For fit strategy (e.g., "256Mi")
	AnnotationAdjusted = "nodefit.io/adjusted" // Marker that limits were adjusted

	// Strategies
	StrategyPercent = "percent"  // limit = min(original, percent% of node_allocatable / pods_count)
	StrategyFit     = "fit"      // limit = node_allocatable - sum(other_pods_requests) - buffer
	StrategyCap     = "cap"      // limit = request (no burst allowed)

	// Defaults
	DefaultStrategy = StrategyPercent
	DefaultPercent  = 80
	DefaultBuffer   = "256Mi"
)

// PodReconciler reconciles Pods with nodefit.io annotations
type PodReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups="",resources=nodes,verbs=get;list;watch

// Reconcile handles pod reconciliation
func (r *PodReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// Fetch the Pod
	var pod corev1.Pod
	if err := r.Get(ctx, req.NamespacedName, &pod); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Check if nodefit is enabled via annotation
	if !isNodeFitEnabled(&pod) {
		return ctrl.Result{}, nil
	}

	// Skip if pod is not running or not scheduled
	if pod.Spec.NodeName == "" {
		logger.V(1).Info("Pod not yet scheduled, skipping", "pod", req.NamespacedName)
		return ctrl.Result{}, nil
	}

	if pod.Status.Phase != corev1.PodRunning {
		logger.V(1).Info("Pod not running, skipping", "pod", req.NamespacedName, "phase", pod.Status.Phase)
		return ctrl.Result{}, nil
	}

	// Fetch the Node
	var node corev1.Node
	if err := r.Get(ctx, types.NamespacedName{Name: pod.Spec.NodeName}, &node); err != nil {
		logger.Error(err, "Failed to get node", "node", pod.Spec.NodeName)
		return ctrl.Result{}, err
	}

	// Get configuration from annotations
	config := getConfig(&pod)
	logger.Info("Processing pod", "pod", req.NamespacedName, "node", pod.Spec.NodeName, "strategy", config.Strategy)

	// Calculate new limits based on strategy
	newLimits, err := r.calculateLimits(ctx, &pod, &node, config)
	if err != nil {
		logger.Error(err, "Failed to calculate limits")
		return ctrl.Result{}, err
	}

	// Check if we need to update
	if !needsUpdate(&pod, newLimits) {
		logger.V(1).Info("No update needed", "pod", req.NamespacedName)
		return ctrl.Result{}, nil
	}

	// Patch the pod with new limits (in-place resize - K8s 1.35+)
	if err := r.patchPodLimits(ctx, &pod, newLimits); err != nil {
		logger.Error(err, "Failed to patch pod limits")
		return ctrl.Result{}, err
	}

	logger.Info("Successfully adjusted pod limits",
		"pod", req.NamespacedName,
		"strategy", config.Strategy,
		"newMemoryLimit", newLimits.Memory().String(),
		"newCPULimit", newLimits.Cpu().String())

	return ctrl.Result{}, nil
}

// Config holds the nodefit configuration from annotations
type Config struct {
	Strategy string
	Percent  int
	Buffer   resource.Quantity
}

func isNodeFitEnabled(pod *corev1.Pod) bool {
	if pod.Annotations == nil {
		return false
	}
	enabled, ok := pod.Annotations[AnnotationEnabled]
	return ok && strings.ToLower(enabled) == "true"
}

func getConfig(pod *corev1.Pod) Config {
	config := Config{
		Strategy: DefaultStrategy,
		Percent:  DefaultPercent,
		Buffer:   resource.MustParse(DefaultBuffer),
	}

	if pod.Annotations == nil {
		return config
	}

	if strategy, ok := pod.Annotations[AnnotationStrategy]; ok {
		switch strings.ToLower(strategy) {
		case StrategyPercent, StrategyFit, StrategyCap:
			config.Strategy = strings.ToLower(strategy)
		}
	}

	if percentStr, ok := pod.Annotations[AnnotationPercent]; ok {
		if percent, err := strconv.Atoi(percentStr); err == nil && percent > 0 && percent <= 100 {
			config.Percent = percent
		}
	}

	if bufferStr, ok := pod.Annotations[AnnotationBuffer]; ok {
		if buffer, err := resource.ParseQuantity(bufferStr); err == nil {
			config.Buffer = buffer
		}
	}

	return config
}

func (r *PodReconciler) calculateLimits(ctx context.Context, pod *corev1.Pod, node *corev1.Node, config Config) (corev1.ResourceList, error) {
	switch config.Strategy {
	case StrategyPercent:
		return r.calculatePercentLimits(ctx, pod, node, config.Percent)
	case StrategyFit:
		return r.calculateFitLimits(ctx, pod, node, config.Buffer)
	case StrategyCap:
		return r.calculateCapLimits(pod)
	default:
		return nil, fmt.Errorf("unknown strategy: %s", config.Strategy)
	}
}

// calculatePercentLimits: limit = min(original_limit, percent% of node_allocatable / pods_on_node)
func (r *PodReconciler) calculatePercentLimits(ctx context.Context, pod *corev1.Pod, node *corev1.Node, percent int) (corev1.ResourceList, error) {
	newLimits := make(corev1.ResourceList)

	// Count pods on this node
	podCount, err := r.countPodsOnNode(ctx, node.Name)
	if err != nil {
		return newLimits, err
	}
	if podCount == 0 {
		podCount = 1
	}

	allocatable := node.Status.Allocatable

	// Calculate memory limit
	if allocatableMem, ok := allocatable[corev1.ResourceMemory]; ok {
		// (allocatable * percent / 100) / podCount
		maxMemBytes := allocatableMem.Value() * int64(percent) / 100 / int64(podCount)
		maxMem := resource.NewQuantity(maxMemBytes, resource.BinarySI)

		// Get current limit
		currentLimit := getCurrentLimit(pod, corev1.ResourceMemory)
		if currentLimit != nil && currentLimit.Value() > 0 {
			// Use minimum of current limit and calculated max
			if maxMem.Value() < currentLimit.Value() {
				newLimits[corev1.ResourceMemory] = *maxMem
			} else {
				newLimits[corev1.ResourceMemory] = *currentLimit
			}
		} else {
			newLimits[corev1.ResourceMemory] = *maxMem
		}
	}

	// Calculate CPU limit (similar logic)
	if allocatableCPU, ok := allocatable[corev1.ResourceCPU]; ok {
		maxCPUMilli := allocatableCPU.MilliValue() * int64(percent) / 100 / int64(podCount)
		maxCPU := resource.NewMilliQuantity(maxCPUMilli, resource.DecimalSI)

		currentLimit := getCurrentLimit(pod, corev1.ResourceCPU)
		if currentLimit != nil && currentLimit.MilliValue() > 0 {
			if maxCPU.MilliValue() < currentLimit.MilliValue() {
				newLimits[corev1.ResourceCPU] = *maxCPU
			} else {
				newLimits[corev1.ResourceCPU] = *currentLimit
			}
		}
		// Don't set CPU limit if not already set (best practice)
	}

	return newLimits, nil
}

// calculateFitLimits: limit = node_allocatable - sum(other_pods_requests) - buffer
func (r *PodReconciler) calculateFitLimits(ctx context.Context, pod *corev1.Pod, node *corev1.Node, buffer resource.Quantity) (corev1.ResourceList, error) {
	newLimits := make(corev1.ResourceList)

	// Get all pods on this node
	var podList corev1.PodList
	if err := r.List(ctx, &podList, client.MatchingFields{"spec.nodeName": node.Name}); err != nil {
		return newLimits, err
	}

	// Sum requests of other pods
	var otherPodsMemRequests int64
	var otherPodsCPURequests int64
	for _, p := range podList.Items {
		if p.UID == pod.UID {
			continue // Skip our own pod
		}
		if p.Status.Phase != corev1.PodRunning && p.Status.Phase != corev1.PodPending {
			continue
		}
		for _, container := range p.Spec.Containers {
			if mem, ok := container.Resources.Requests[corev1.ResourceMemory]; ok {
				otherPodsMemRequests += mem.Value()
			}
			if cpu, ok := container.Resources.Requests[corev1.ResourceCPU]; ok {
				otherPodsCPURequests += cpu.MilliValue()
			}
		}
	}

	allocatable := node.Status.Allocatable

	// Calculate available memory
	if allocatableMem, ok := allocatable[corev1.ResourceMemory]; ok {
		availableMem := allocatableMem.Value() - otherPodsMemRequests - buffer.Value()
		if availableMem < 0 {
			availableMem = 0
		}
		newLimits[corev1.ResourceMemory] = *resource.NewQuantity(availableMem, resource.BinarySI)
	}

	// Calculate available CPU
	if allocatableCPU, ok := allocatable[corev1.ResourceCPU]; ok {
		availableCPU := allocatableCPU.MilliValue() - otherPodsCPURequests
		if availableCPU < 0 {
			availableCPU = 0
		}
		newLimits[corev1.ResourceCPU] = *resource.NewMilliQuantity(availableCPU, resource.DecimalSI)
	}

	return newLimits, nil
}

// calculateCapLimits: limit = request (no burst)
func (r *PodReconciler) calculateCapLimits(pod *corev1.Pod) (corev1.ResourceList, error) {
	newLimits := make(corev1.ResourceList)

	for _, container := range pod.Spec.Containers {
		if mem, ok := container.Resources.Requests[corev1.ResourceMemory]; ok {
			newLimits[corev1.ResourceMemory] = mem
		}
		if cpu, ok := container.Resources.Requests[corev1.ResourceCPU]; ok {
			newLimits[corev1.ResourceCPU] = cpu
		}
		// Only process first container for now
		break
	}

	return newLimits, nil
}

func (r *PodReconciler) countPodsOnNode(ctx context.Context, nodeName string) (int, error) {
	var podList corev1.PodList
	if err := r.List(ctx, &podList, client.MatchingFields{"spec.nodeName": nodeName}); err != nil {
		return 0, err
	}

	count := 0
	for _, p := range podList.Items {
		if p.Status.Phase == corev1.PodRunning || p.Status.Phase == corev1.PodPending {
			count++
		}
	}
	return count, nil
}

func getCurrentLimit(pod *corev1.Pod, resourceName corev1.ResourceName) *resource.Quantity {
	for _, container := range pod.Spec.Containers {
		if limit, ok := container.Resources.Limits[resourceName]; ok {
			return &limit
		}
	}
	return nil
}

func needsUpdate(pod *corev1.Pod, newLimits corev1.ResourceList) bool {
	if len(newLimits) == 0 {
		return false
	}

	for _, container := range pod.Spec.Containers {
		for resourceName, newLimit := range newLimits {
			currentLimit, ok := container.Resources.Limits[resourceName]
			if !ok || !currentLimit.Equal(newLimit) {
				return true
			}
		}
	}
	return false
}

func (r *PodReconciler) patchPodLimits(ctx context.Context, pod *corev1.Pod, newLimits corev1.ResourceList) error {
	patch := client.MergeFrom(pod.DeepCopy())

	// Update limits for all containers
	for i := range pod.Spec.Containers {
		if pod.Spec.Containers[i].Resources.Limits == nil {
			pod.Spec.Containers[i].Resources.Limits = make(corev1.ResourceList)
		}
		for resourceName, limit := range newLimits {
			pod.Spec.Containers[i].Resources.Limits[resourceName] = limit
		}
	}

	// Add adjusted annotation
	if pod.Annotations == nil {
		pod.Annotations = make(map[string]string)
	}
	pod.Annotations[AnnotationAdjusted] = "true"

	return r.Patch(ctx, pod, patch)
}

// SetupWithManager sets up the controller with the Manager.
func (r *PodReconciler) SetupWithManager(mgr ctrl.Manager) error {
	// Create an index for pods by node name
	if err := mgr.GetFieldIndexer().IndexField(context.Background(), &corev1.Pod{}, "spec.nodeName", func(rawObj client.Object) []string {
		pod := rawObj.(*corev1.Pod)
		return []string{pod.Spec.NodeName}
	}); err != nil {
		return err
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(&corev1.Pod{}).
		Named("nodefit-pod").
		Complete(r)
}
