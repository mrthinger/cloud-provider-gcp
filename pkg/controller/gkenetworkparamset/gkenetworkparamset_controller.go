/*
Copyright 2022 The Kubernetes Authors.

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

package gkenetworkparamset

import (
	"context"
	"errors"
	"fmt"
	"time"

	"google.golang.org/api/compute/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"

	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"

	networkv1 "k8s.io/cloud-provider-gcp/crd/apis/network/v1"
	networkv1alpha1 "k8s.io/cloud-provider-gcp/crd/apis/network/v1alpha1"
	networkclientset "k8s.io/cloud-provider-gcp/crd/client/network/clientset/versioned"
	"k8s.io/cloud-provider-gcp/providers/gce"

	controllersmetrics "k8s.io/component-base/metrics/prometheus/controllers"
)

type GNPError string

const (
	GNPKind                  = "GKENetworkParamSet"
	GNPFinalizer             = "networking.gke.io/ccm"
	ParamsReadyConditionType = "ParamsReady"
	ReadyConditionType       = "Ready"

	// Validation Types
	SubnetNotFound                         = "SubnetNotFound"
	SecondaryRangeNotFound                 = "SecondaryRangeNotFound"
	DeviceModeCantBeUsedWithSecondaryRange = "DeviceModeCantBeUsedWithSecondaryRange"
	DeviceModeVPCAlreadyInUse              = "DeviceModeVPCAlreadyInUse"
	DeviceModeCantUseDefaultVPC            = "DeviceModeCantUseDefaultVPC"
	L3SecondaryMissing                     = "L3SecondaryMissing"
	L3DeviceModeExists                     = "L3DeviceModeExists"
	DeviceSecondaryExists                  = "DeviceSecondaryExists"
	DeviceModeMissing                      = "DeviceModeMissing"
	DPDKUnsupported                        = "DPDKUnsupported"
)

// Controller manages GKENetworkParamSet status.
type Controller struct {
	gkeNetworkParamsInformer cache.SharedIndexInformer
	networkInformer          cache.SharedIndexInformer
	networkClientset         networkclientset.Interface
	gceCloud                 *gce.Cloud
	queue                    workqueue.RateLimitingInterface
}

// NewGKENetworkParamSetController returns a new
func NewGKENetworkParamSetController(
	networkClientset networkclientset.Interface,
	gkeNetworkParamsInformer cache.SharedIndexInformer,
	networkInformer cache.SharedIndexInformer,
	gceCloud *gce.Cloud,
) *Controller {

	// register GNP metrics
	registerGKENetworkParamSetMetrics()

	return &Controller{
		networkClientset:         networkClientset,
		gkeNetworkParamsInformer: gkeNetworkParamsInformer,
		networkInformer:          networkInformer,
		gceCloud:                 gceCloud,
		queue:                    workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), "gkenetworkparamset"),
	}

}

// Run starts an asynchronous loop that monitors and updates GKENetworkParamSet in the cluster.
func (c *Controller) Run(numWorkers int, stopCh <-chan struct{}, controllerManagerMetrics *controllersmetrics.ControllerManagerMetrics) {
	defer utilruntime.HandleCrash()

	ctx, cancelFn := context.WithCancel(context.Background())
	defer cancelFn()
	defer c.queue.ShutDown()

	klog.Infof("Starting gkenetworkparamset controller")
	defer klog.Infof("Shutting down gkenetworkparamset controller")
	controllerManagerMetrics.ControllerStarted("gkenetworkparamset")
	defer controllerManagerMetrics.ControllerStopped("gkenetworkparamset")

	c.gkeNetworkParamsInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			key, err := cache.MetaNamespaceKeyFunc(obj)
			if err == nil {
				c.queue.Add(key)
			}
		},
		UpdateFunc: func(old interface{}, new interface{}) {
			key, err := cache.MetaNamespaceKeyFunc(new)
			if err == nil {
				c.queue.Add(key)
			}
		},
	})

	c.networkInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			network := obj.(*networkv1.Network)
			if network.Spec.ParametersRef != nil && network.Spec.ParametersRef.Kind == GNPKind {
				c.queue.Add(network.Spec.ParametersRef.Name)
			}
		},
		UpdateFunc: func(old, new interface{}) {
			network := new.(*networkv1.Network)
			if network.Spec.ParametersRef != nil && network.Spec.ParametersRef.Kind == GNPKind {
				c.queue.Add(network.Spec.ParametersRef.Name)
			}
		},
	})

	if !cache.WaitForNamedCacheSync("gkenetworkparamset", stopCh, c.gkeNetworkParamsInformer.HasSynced, c.networkInformer.HasSynced) {
		return
	}
	for i := 0; i < numWorkers; i++ {
		go wait.UntilWithContext(ctx, c.runWorker, time.Second)
	}

	<-stopCh
}

// worker pattern adapted from https://github.com/kubernetes/client-go/blob/master/examples/workqueue/main.go
func (c *Controller) runWorker(ctx context.Context) {
	for c.processNextItem(ctx) {
	}
}

func (c *Controller) processNextItem(ctx context.Context) bool {
	key, quit := c.queue.Get()
	if quit {
		return false
	}

	defer c.queue.Done(key)

	err := c.syncGKENetworkParamSet(ctx, key.(string))
	c.handleErr(err, key)
	return true
}

// handleErr checks if an error happened and makes sure we will retry later.
func (c *Controller) handleErr(err error, key interface{}) {
	if err == nil {
		// Forget about the #AddRateLimited history of the key on every successful synchronization.
		// This ensures that future processing of updates for this key is not delayed because of
		// an outdated error history.
		c.queue.Forget(key)
		return
	}

	// This controller retries 5 times if something goes wrong. After that, it stops trying.
	if c.queue.NumRequeues(key) < 5 {
		klog.Warningf("Error while updating GKENetworkParamSet object, retrying %v: %v", key, err)

		// Re-enqueue the key rate limited. Based on the rate limiter on the
		// queue and the re-enqueue history, the key will be processed later again.
		c.queue.AddRateLimited(key)
		return
	}

	c.queue.Forget(key)
	// Report to an external entity that, even after several retries, we could not successfully process this key
	utilruntime.HandleError(err)
	klog.Errorf("Dropping GKENetworkParamSet %q out of the queue: %v", key, err)
}

type validation struct {
	IsValid      bool
	ErrorReason  string
	ErrorMessage string
}

func (c *Controller) getAndValidateSubnet(ctx context.Context, params *networkv1alpha1.GKENetworkParamSet) (*compute.Subnetwork, *validation) {
	if params.Spec.VPCSubnet == "" {
		return nil, &validation{
			IsValid:      false,
			ErrorReason:  SubnetNotFound,
			ErrorMessage: fmt.Sprintf("subnet not specified"),
		}
	}

	// Check if Subnet exists
	subnet, err := c.gceCloud.GetSubnetwork(c.gceCloud.Region(), params.Spec.VPCSubnet)
	if err != nil || subnet == nil {
		fetchSubnetErrs.Inc()
		return nil, &validation{
			IsValid:      false,
			ErrorReason:  SubnetNotFound,
			ErrorMessage: fmt.Sprintf("%s not found in %s", params.Spec.VPCSubnet, params.Spec.VPC),
		}
	}

	return subnet, &validation{IsValid: true}
}

func (c *Controller) validateGKENetworkParamSet(ctx context.Context, params *networkv1alpha1.GKENetworkParamSet, subnet *compute.Subnetwork) (*validation, error) {

	// Check if secondary range exists
	if params.Spec.PodIPv4Ranges != nil && params.Spec.DeviceMode == "" {
		for _, rangeName := range params.Spec.PodIPv4Ranges.RangeNames {
			found := false
			for _, sr := range subnet.SecondaryIpRanges {
				if sr.RangeName == rangeName {
					found = true
					break
				}
			}
			if !found {
				return &validation{
					IsValid:      false,
					ErrorReason:  SecondaryRangeNotFound,
					ErrorMessage: fmt.Sprintf("%s not found in %s", rangeName, params.Spec.VPCSubnet),
				}, nil
			}
		}
	}

	// Check if deviceMode is specified at the same time as secondary range
	if params.Spec.DeviceMode != "" && params.Spec.PodIPv4Ranges != nil && len(params.Spec.PodIPv4Ranges.RangeNames) > 0 {
		return &validation{
			IsValid:      false,
			ErrorReason:  DeviceModeCantBeUsedWithSecondaryRange,
			ErrorMessage: "deviceMode and secondary range can not be specified at the same time",
		}, nil
	}

	//if GNP with deviceMode and referencing VPC is referenced in any other existing GNP
	if params.Spec.DeviceMode != "" {
		gnpList, err := c.networkClientset.NetworkingV1alpha1().GKENetworkParamSets().List(ctx, v1.ListOptions{})
		if err != nil {
			return nil, err
		}
		for _, otherGNP := range gnpList.Items {
			if params.Name != otherGNP.Name && params.Spec.VPC == otherGNP.Spec.VPC && params.CreationTimestamp.After(otherGNP.CreationTimestamp.Time) {
				return &validation{
					IsValid:      false,
					ErrorReason:  DeviceModeVPCAlreadyInUse,
					ErrorMessage: fmt.Sprintf("GNP with deviceMode can't reference a VPC already in use. VPC '%s' is already in use by '%s'", otherGNP.Spec.VPC, otherGNP.Name),
				}, nil
			}
		}
	}

	//if GNP with deviceMode and The referencing VPC is the default VPC
	if params.Spec.DeviceMode != "" && params.Spec.VPC == c.gceCloud.NetworkName() {
		return &validation{
			IsValid:      false,
			ErrorReason:  DeviceModeCantUseDefaultVPC,
			ErrorMessage: "GNP with deviceMode can't reference the default VPC",
		}, nil
	}

	return &validation{IsValid: true}, nil
}

func validationResultToCondition(validationResult *validation) v1.Condition {
	condition := v1.Condition{}

	if validationResult.IsValid {
		condition.Status = v1.ConditionTrue
	} else {
		condition.Status = v1.ConditionFalse
		condition.Reason = validationResult.ErrorReason
		condition.Message = validationResult.ErrorMessage
	}

	return condition
}

// addFinalizerToGKENetworkParamSet add a finalizer to params if it doesnt already exist
func (c *Controller) addFinalizerToGKENetworkParamSet(ctx context.Context, params *networkv1alpha1.GKENetworkParamSet) error {
	for _, f := range params.ObjectMeta.Finalizers {
		if f == GNPFinalizer {
			return nil
		}
	}

	params.ObjectMeta.Finalizers = append(params.ObjectMeta.Finalizers, GNPFinalizer)
	return nil
}

func (c *Controller) syncGKENetworkParamSet(ctx context.Context, key string) (err error) {
	obj, exists, err := c.gkeNetworkParamsInformer.GetIndexer().GetByKey(key)
	if err != nil {
		klog.Errorf("Fetching object with key %s from store failed with %v", key, err)
		return err
	}

	if !exists {
		// GKENetworkParamSet does not exist anymore since the work was queued, so move on
		return nil
	}

	params := obj.(*networkv1alpha1.GKENetworkParamSet)

	defer func() {
		// diff objects before updating
		if updateErr := c.updateGKENetworkParamSet(ctx, params); updateErr != nil {
			err = errors.Join(updateErr, err)
		}
		if updateErr := c.updateGKENetworkParamSetStatus(ctx, params); updateErr != nil {
			err = errors.Join(updateErr, err)
		}
	}()

	// Place a finalizer on GNP
	err = c.addFinalizerToGKENetworkParamSet(ctx, params)
	if err != nil {
		return
	}

	cidrs := []string{}

	// Get and validate the subnet
	subnet, subnetValidationResult := c.getAndValidateSubnet(ctx, params)
	if !subnetValidationResult.IsValid {
		c.transformGKENetworkParamSetStatus(params, cidrs, subnetValidationResult)
		return
	}

	gnpValidationResult, err := c.validateGKENetworkParamSet(ctx, params, subnet)
	if err != nil {
		return
	}
	if !gnpValidationResult.IsValid {
		c.transformGKENetworkParamSetStatus(params, cidrs, gnpValidationResult)
		return
	}

	cidrs = extractRelevantCidrs(subnet, params)
	c.transformGKENetworkParamSetStatus(params, cidrs, &validation{IsValid: true})

	// list networks
	networks, err := c.networkClientset.NetworkingV1().Networks().List(ctx, v1.ListOptions{})
	if err != nil {
		return
	}
	// see if one of the networks is referencing this GNP
	for _, network := range networks.Items {
		if network.Spec.ParametersRef.Name == params.Name && network.Spec.ParametersRef.Kind == GNPKind {
			// cross validate network and gnp
			validationResult := crossValidateNetworkAndGnp(network, params)

			// update network conditions
			err = c.updateNetworkConditions(ctx, &network, validationResult)
			if err != nil {
				return
			}

			// update networkName in GNP status if everything is valid
			if validationResult.IsValid {
				params.Status.NetworkName = network.Name
			}
		}
	}

	return
}

func (c *Controller) updateNetworkConditions(ctx context.Context, network *networkv1.Network, validationResult *validation) error {
	condition := validationResultToCondition(validationResult)

	condition.Type = ParamsReadyConditionType
	condition.LastTransitionTime = v1.NewTime(time.Now())

	// Update the existing condition, or append it if it does not exist
	updated := false
	for i, existingCondition := range network.Status.Conditions {
		if existingCondition.Type == condition.Type {
			network.Status.Conditions[i] = condition
			updated = true
			break
		}
	}
	if !updated {
		network.Status.Conditions = append(network.Status.Conditions, condition)
	}

	_, err := c.networkClientset.NetworkingV1().Networks().UpdateStatus(ctx, network, v1.UpdateOptions{})
	if err != nil {
		return fmt.Errorf("failed to update network conditions: %v", err)
	}
	return nil
}

func crossValidateNetworkAndGnp(network networkv1.Network, params *networkv1alpha1.GKENetworkParamSet) *validation {
	if network.Spec.Type == networkv1.L3NetworkType {
		if params.Spec.PodIPv4Ranges == nil {
			return &validation{
				IsValid:      false,
				ErrorReason:  L3SecondaryMissing,
				ErrorMessage: "L3 type network requires secondary range to be specified in params",
			}
		}
		if params.Spec.DeviceMode != "" {
			return &validation{
				IsValid:      false,
				ErrorReason:  L3DeviceModeExists,
				ErrorMessage: "L3 type network can't be used with a device mode specified in params",
			}
		}
	}

	if network.Spec.Type == networkv1.DeviceNetworkType {
		if params.Spec.DeviceMode == "" {
			return &validation{
				IsValid:      false,
				ErrorReason:  DeviceModeMissing,
				ErrorMessage: "Device type network requires device mode to be specified in params",
			}
		}
		if params.Spec.PodIPv4Ranges != nil {
			return &validation{
				IsValid:      false,
				ErrorReason:  DeviceSecondaryExists,
				ErrorMessage: "Device type network can't be used with a secondary range specified in params",
			}
		}
	}

	// All conditions have passed
	return &validation{
		IsValid: true,
	}
}

// extractRelevantCidrs returns the CIDRS of the named ranges in paramset
func extractRelevantCidrs(subnet *compute.Subnetwork, paramset *networkv1alpha1.GKENetworkParamSet) []string {
	cidrs := []string{}

	// use the subnet cidr if there are no secondary ranges specified by user in params
	if paramset.Spec.PodIPv4Ranges == nil || (paramset.Spec.PodIPv4Ranges != nil && len(paramset.Spec.PodIPv4Ranges.RangeNames) == 0) {
		cidrs = append(cidrs, subnet.IpCidrRange)
		return cidrs
	}

	// get secondary ranges' cooresponding cidrs
	for _, sr := range subnet.SecondaryIpRanges {
		if !paramSetIncludesRange(paramset, sr.RangeName) {
			continue
		}

		cidrs = append(cidrs, sr.IpCidrRange)
	}
	return cidrs
}

func paramSetIncludesRange(params *networkv1alpha1.GKENetworkParamSet, secondaryRangeName string) bool {
	for _, rn := range params.Spec.PodIPv4Ranges.RangeNames {
		if rn == secondaryRangeName {
			return true
		}
	}
	return false
}

// updateGKENetworkParamSet performs a update for the given GKENetworkParamSet
func (c *Controller) updateGKENetworkParamSet(ctx context.Context, params *networkv1alpha1.GKENetworkParamSet) error {
	_, err := c.networkClientset.NetworkingV1alpha1().GKENetworkParamSets().Update(ctx, params, v1.UpdateOptions{})
	if err != nil {
		return fmt.Errorf("failed to update GKENetworkParamSet: %v", err)
	}
	return nil
}

func (c *Controller) transformGKENetworkParamSetStatus(gkeNetworkParamSet *networkv1alpha1.GKENetworkParamSet, cidrs []string, validationResult *validation) {
	condition := validationResultToCondition(validationResult)

	condition.Type = ReadyConditionType
	condition.LastTransitionTime = v1.NewTime(time.Now())

	// Update the existing condition, or append it if it does not exist
	updated := false
	for i, existingCondition := range gkeNetworkParamSet.Status.Conditions {
		if existingCondition.Type == condition.Type {
			gkeNetworkParamSet.Status.Conditions[i] = condition
			updated = true
			break
		}
	}
	if !updated {
		gkeNetworkParamSet.Status.Conditions = append(gkeNetworkParamSet.Status.Conditions, condition)
	}

	gkeNetworkParamSet.Status.PodCIDRs = &networkv1alpha1.NetworkRanges{
		CIDRBlocks: cidrs,
	}
}

// updateGKENetworkParamSetStatus performs a status update for the given GKENetworkParamSet on the cluster
func (c *Controller) updateGKENetworkParamSetStatus(ctx context.Context, gkeNetworkParamSet *networkv1alpha1.GKENetworkParamSet) error {

	_, err := c.networkClientset.NetworkingV1alpha1().GKENetworkParamSets().UpdateStatus(ctx, gkeNetworkParamSet, v1.UpdateOptions{})
	if err != nil {
		return fmt.Errorf("failed to update GKENetworkParamSet Status: %v", err)
	}
	return nil
}
