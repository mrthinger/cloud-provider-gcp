package gkenetworkparamset

import (
	"context"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/k8s-cloud-provider/pkg/cloud/meta"
	"github.com/onsi/gomega"
	"google.golang.org/api/compute/v1"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/cache"
	networkv1 "k8s.io/cloud-provider-gcp/crd/apis/network/v1"
	"k8s.io/cloud-provider-gcp/crd/apis/network/v1alpha1"
	"k8s.io/cloud-provider-gcp/crd/client/network/clientset/versioned/fake"
	v1informers "k8s.io/cloud-provider-gcp/crd/client/network/informers/externalversions/network/v1"
	v1alphainformers "k8s.io/cloud-provider-gcp/crd/client/network/informers/externalversions/network/v1alpha1"
	"k8s.io/cloud-provider-gcp/providers/gce"
	"k8s.io/component-base/metrics/prometheus/controllers"
)

type testGKENetworkParamSetController struct {
	ctx             context.Context
	stop            context.CancelFunc
	networkClient   *fake.Clientset
	gnpInformer     cache.SharedIndexInformer
	networkInformer cache.SharedIndexInformer
	clusterValues   gce.TestClusterValues
	controller      *Controller
	metrics         *controllers.ControllerManagerMetrics
	cloud           *gce.Cloud
}

func setupGKENetworkParamSetController() *testGKENetworkParamSetController {
	fakeNetworking := fake.NewSimpleClientset()
	gkeNetworkParamSetInformer := v1alphainformers.NewGKENetworkParamSetInformer(fakeNetworking, 0*time.Second, cache.Indexers{})
	networkInformer := v1informers.NewNetworkInformer(fakeNetworking, 0*time.Second, cache.Indexers{})
	testClusterValues := gce.DefaultTestClusterValues()
	fakeGCE := gce.NewFakeGCECloud(testClusterValues)
	controller := NewGKENetworkParamSetController(
		fakeNetworking,
		gkeNetworkParamSetInformer,
		networkInformer,
		fakeGCE,
	)
	metrics := controllers.NewControllerManagerMetrics("test")

	return &testGKENetworkParamSetController{
		networkClient:   fakeNetworking,
		gnpInformer:     gkeNetworkParamSetInformer,
		networkInformer: networkInformer,
		clusterValues:   testClusterValues,
		controller:      controller,
		metrics:         metrics,
		cloud:           fakeGCE,
	}
}

func (testVals *testGKENetworkParamSetController) runGKENetworkParamSetController(ctx context.Context) {
	go testVals.gnpInformer.Run(ctx.Done())
	go testVals.networkInformer.Run(ctx.Done())
	go testVals.controller.Run(1, ctx.Done(), testVals.metrics)
}

func TestControllerRuns(t *testing.T) {
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	testVals := setupGKENetworkParamSetController()
	testVals.runGKENetworkParamSetController(ctx)
}

func TestAddValidParamSetSingleSecondaryRange(t *testing.T) {
	g := gomega.NewGomegaWithT(t)
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	testVals := setupGKENetworkParamSetController()

	subnetName := "test-subnet"
	subnetSecondaryRangeName := "test-secondary-range"
	subnetSecondaryCidr := "10.0.0.0/24"
	subnetKey := meta.RegionalKey(subnetName, testVals.clusterValues.Region)
	subnet := &compute.Subnetwork{
		Name: subnetName,
		SecondaryIpRanges: []*compute.SubnetworkSecondaryRange{
			{
				IpCidrRange: subnetSecondaryCidr,
				RangeName:   subnetSecondaryRangeName,
			},
		},
	}

	err := testVals.cloud.Compute().Subnetworks().Insert(ctx, subnetKey, subnet)
	if err != nil {
		t.Error(err)
	}

	testVals.runGKENetworkParamSetController(ctx)

	gkeNetworkParamSetName := "test-paramset"
	paramSet := &v1alpha1.GKENetworkParamSet{
		ObjectMeta: v1.ObjectMeta{
			Name: gkeNetworkParamSetName,
		},
		Spec: v1alpha1.GKENetworkParamSetSpec{
			VPC:       "default",
			VPCSubnet: subnetName,
			PodIPv4Ranges: &v1alpha1.SecondaryRanges{
				RangeNames: []string{
					subnetSecondaryRangeName,
				},
			},
		},
	}
	_, err = testVals.networkClient.NetworkingV1alpha1().GKENetworkParamSets().Create(ctx, paramSet, v1.CreateOptions{})
	if err != nil {
		t.Error(err)
	}

	g.Eventually(func() (bool, error) {
		paramSet, err := testVals.networkClient.NetworkingV1alpha1().GKENetworkParamSets().Get(ctx, gkeNetworkParamSetName, v1.GetOptions{})
		if err != nil {
			return false, err
		}

		cidrExists := paramSet.Status.PodCIDRs != nil && len(paramSet.Status.PodCIDRs.CIDRBlocks) > 0
		if cidrExists {
			g.Ω(paramSet.Status.PodCIDRs.CIDRBlocks).Should(gomega.ConsistOf(subnetSecondaryCidr))
			return true, nil
		}

		return false, nil
	}).Should(gomega.BeTrue(), "GKENetworkParamSet Status should be updated with secondary range cidr.")

}

func TestAddValidParamSetMultipleSecondaryRange(t *testing.T) {
	g := gomega.NewGomegaWithT(t)
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	testVals := setupGKENetworkParamSetController()

	subnetName := "test-subnet"
	subnetSecondaryRangeName1 := "test-secondary-range-1"
	subnetSecondaryCidr1 := "10.0.0.1/24"
	subnetSecondaryRangeName2 := "test-secondary-range-2"
	subnetSecondaryCidr2 := "10.0.0.2/24"
	subnetKey := meta.RegionalKey(subnetName, testVals.clusterValues.Region)
	subnet := &compute.Subnetwork{
		Name: subnetName,
		SecondaryIpRanges: []*compute.SubnetworkSecondaryRange{
			{
				IpCidrRange: subnetSecondaryCidr1,
				RangeName:   subnetSecondaryRangeName1,
			},
			{
				IpCidrRange: subnetSecondaryCidr2,
				RangeName:   subnetSecondaryRangeName2,
			},
		},
	}

	err := testVals.cloud.Compute().Subnetworks().Insert(ctx, subnetKey, subnet)
	if err != nil {
		t.Error(err)
	}

	testVals.runGKENetworkParamSetController(ctx)

	gkeNetworkParamSetName := "test-paramset"
	paramSet := &v1alpha1.GKENetworkParamSet{
		ObjectMeta: v1.ObjectMeta{
			Name: gkeNetworkParamSetName,
		},
		Spec: v1alpha1.GKENetworkParamSetSpec{
			VPC:       "default",
			VPCSubnet: subnetName,
			PodIPv4Ranges: &v1alpha1.SecondaryRanges{
				RangeNames: []string{
					subnetSecondaryRangeName1,
					subnetSecondaryRangeName2,
				},
			},
		},
	}
	_, err = testVals.networkClient.NetworkingV1alpha1().GKENetworkParamSets().Create(ctx, paramSet, v1.CreateOptions{})
	if err != nil {
		t.Error(err)
	}

	g.Eventually(func() (bool, error) {
		paramSet, err := testVals.networkClient.NetworkingV1alpha1().GKENetworkParamSets().Get(ctx, gkeNetworkParamSetName, v1.GetOptions{})
		if err != nil {
			return false, err
		}

		cidrExists := paramSet.Status.PodCIDRs != nil && len(paramSet.Status.PodCIDRs.CIDRBlocks) > 0
		if cidrExists {
			g.Ω(paramSet.Status.PodCIDRs.CIDRBlocks).Should(gomega.ConsistOf(subnetSecondaryCidr1, subnetSecondaryCidr2))
			return true, nil
		}

		return false, nil
	}).Should(gomega.BeTrue(), "GKENetworkParamSet Status should be updated with secondary range cidrs.")

}

func TestAddInvalidParamSetNoMatchingSecondaryRange(t *testing.T) {
	g := gomega.NewGomegaWithT(t)
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	testVals := setupGKENetworkParamSetController()

	subnetName := "test-subnet"
	subnetKey := meta.RegionalKey(subnetName, testVals.clusterValues.Region)
	subnet := &compute.Subnetwork{
		Name: subnetName,
	}

	err := testVals.cloud.Compute().Subnetworks().Insert(ctx, subnetKey, subnet)
	if err != nil {
		t.Error(err)
	}

	testVals.runGKENetworkParamSetController(ctx)

	gkeNetworkParamSetName := "test-paramset"
	nonExistentSecondaryRangeName := "test-secondary-does-not-exist"
	paramSet := &v1alpha1.GKENetworkParamSet{
		ObjectMeta: v1.ObjectMeta{
			Name: gkeNetworkParamSetName,
		},
		Spec: v1alpha1.GKENetworkParamSetSpec{
			VPC:       "default",
			VPCSubnet: subnetName,
			PodIPv4Ranges: &v1alpha1.SecondaryRanges{
				RangeNames: []string{
					nonExistentSecondaryRangeName,
				},
			},
		},
	}
	_, err = testVals.networkClient.NetworkingV1alpha1().GKENetworkParamSets().Create(ctx, paramSet, v1.CreateOptions{})
	if err != nil {
		t.Error(err)
	}

	g.Consistently(func() (bool, error) {
		paramSet, err := testVals.networkClient.NetworkingV1alpha1().GKENetworkParamSets().Get(ctx, gkeNetworkParamSetName, v1.GetOptions{})
		if err != nil {
			return false, err
		}

		cidrExists := paramSet.Status.PodCIDRs != nil && len(paramSet.Status.PodCIDRs.CIDRBlocks) > 0
		if cidrExists {
			return false, nil
		}

		return true, nil
	}).Should(gomega.BeTrue(), "GKENetworkParamSet Status should contain an empty list of cidrs")

}

func TestParamSetPartialSecondaryRange(t *testing.T) {
	g := gomega.NewGomegaWithT(t)
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	testVals := setupGKENetworkParamSetController()

	subnetName := "test-subnet"
	subnetSecondaryRangeName1 := "test-secondary-range-1"
	subnetSecondaryCidr1 := "10.0.0.1/24"
	subnetKey := meta.RegionalKey(subnetName, testVals.clusterValues.Region)
	subnet := &compute.Subnetwork{
		Name: subnetName,
		SecondaryIpRanges: []*compute.SubnetworkSecondaryRange{
			{
				IpCidrRange: subnetSecondaryCidr1,
				RangeName:   subnetSecondaryRangeName1,
			},
		},
	}

	err := testVals.cloud.Compute().Subnetworks().Insert(ctx, subnetKey, subnet)
	if err != nil {
		t.Error(err)
	}

	testVals.runGKENetworkParamSetController(ctx)

	gkeNetworkParamSetName := "test-paramset"
	nonExistentSecondaryRangeName := "test-secondary-does-not-exist"
	paramSet := &v1alpha1.GKENetworkParamSet{
		ObjectMeta: v1.ObjectMeta{
			Name: gkeNetworkParamSetName,
		},
		Spec: v1alpha1.GKENetworkParamSetSpec{
			VPC:       "default",
			VPCSubnet: subnetName,
			PodIPv4Ranges: &v1alpha1.SecondaryRanges{
				RangeNames: []string{
					subnetSecondaryRangeName1,
					nonExistentSecondaryRangeName,
				},
			},
		},
	}
	_, err = testVals.networkClient.NetworkingV1alpha1().GKENetworkParamSets().Create(ctx, paramSet, v1.CreateOptions{})
	if err != nil {
		t.Error(err)
	}

	g.Eventually(func() (bool, error) {
		paramSet, err := testVals.networkClient.NetworkingV1alpha1().GKENetworkParamSets().Get(ctx, gkeNetworkParamSetName, v1.GetOptions{})
		if err != nil {
			return false, err
		}

		cidrExists := paramSet.Status.PodCIDRs != nil && len(paramSet.Status.PodCIDRs.CIDRBlocks) > 0
		if cidrExists {
			g.Ω(paramSet.Status.PodCIDRs.CIDRBlocks).Should(gomega.ConsistOf(subnetSecondaryCidr1))
			return true, nil
		}

		return false, nil
	}).Should(gomega.BeTrue(), "GKENetworkParamSet Status should be updated with secondary range cidr.")

}

func TestValidParamSetSubnetRange(t *testing.T) {
	g := gomega.NewGomegaWithT(t)
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	testVals := setupGKENetworkParamSetController()

	subnetName := "test-subnet"
	subnetCidr := "10.0.0.0/24"
	subnetKey := meta.RegionalKey(subnetName, testVals.clusterValues.Region)
	subnet := &compute.Subnetwork{
		Name:        subnetName,
		IpCidrRange: subnetCidr,
	}

	err := testVals.cloud.Compute().Subnetworks().Insert(ctx, subnetKey, subnet)
	if err != nil {
		t.Error(err)
	}
	testVals.runGKENetworkParamSetController(ctx)

	gkeNetworkParamSetName := "test-paramset"
	paramSet := &v1alpha1.GKENetworkParamSet{
		ObjectMeta: v1.ObjectMeta{
			Name: gkeNetworkParamSetName,
		},
		Spec: v1alpha1.GKENetworkParamSetSpec{
			VPC:       "default",
			VPCSubnet: subnetName,
		},
	}
	_, err = testVals.networkClient.NetworkingV1alpha1().GKENetworkParamSets().Create(ctx, paramSet, v1.CreateOptions{})
	if err != nil {
		t.Error(err)
	}

	g.Eventually(func() (bool, error) {
		paramSet, err := testVals.networkClient.NetworkingV1alpha1().GKENetworkParamSets().Get(ctx, gkeNetworkParamSetName, v1.GetOptions{})
		if err != nil {
			return false, err
		}

		cidrExists := paramSet.Status.PodCIDRs != nil && len(paramSet.Status.PodCIDRs.CIDRBlocks) > 0
		if cidrExists {
			g.Ω(paramSet.Status.PodCIDRs.CIDRBlocks).Should(gomega.ConsistOf(subnetCidr))
			return true, nil
		}

		return false, nil
	}).Should(gomega.BeTrue(), "GKENetworkParamSet Status should be updated with subnet cidr.")

}

func TestAddFinalizerToGKENetworkParamSet(t *testing.T) {
	g := gomega.NewGomegaWithT(t)
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	testVals := setupGKENetworkParamSetController()

	testVals.runGKENetworkParamSetController(ctx)

	gkeNetworkParamSetName := "test-paramset"
	paramSet := &v1alpha1.GKENetworkParamSet{
		ObjectMeta: v1.ObjectMeta{
			Name: gkeNetworkParamSetName,
		},
		Spec: v1alpha1.GKENetworkParamSetSpec{
			VPC:       "default",
			VPCSubnet: "test-subnet",
		},
	}
	_, err := testVals.networkClient.NetworkingV1alpha1().GKENetworkParamSets().Create(ctx, paramSet, v1.CreateOptions{})
	if err != nil {
		t.Error(err)
	}

	g.Eventually(func() (bool, error) {
		paramSet, err := testVals.networkClient.NetworkingV1alpha1().GKENetworkParamSets().Get(ctx, gkeNetworkParamSetName, v1.GetOptions{})
		if err != nil {
			return false, err
		}

		finalizerExists := false
		for _, finalizer := range paramSet.ObjectMeta.Finalizers {
			if finalizer == GNPFinalizer {
				finalizerExists = true
				break
			}
		}

		return finalizerExists, nil
	}).Should(gomega.BeTrue(), "GKENetworkParamSet should have the finalizer added.")
}

func TestCrossValidateNetworkAndGnp(t *testing.T) {

	// Create a GKENetworkParamSet
	gkeNetworkParamSetName := "test-paramset"
	paramSet := &v1alpha1.GKENetworkParamSet{
		ObjectMeta: v1.ObjectMeta{
			Name: gkeNetworkParamSetName,
		},
		Spec: v1alpha1.GKENetworkParamSetSpec{
			VPC:       "default",
			VPCSubnet: "test-subnet",
		},
	}

	tests := []struct {
		name           string
		network        *networkv1.Network
		paramSet       *v1alpha1.GKENetworkParamSet
		expectedResult *validation
	}{
		{
			name: "L3NetworkType with missing PodIPv4Ranges",
			network: &networkv1.Network{
				ObjectMeta: v1.ObjectMeta{
					Name: "test-network1",
				},
				Spec: networkv1.NetworkSpec{
					Type:          networkv1.L3NetworkType,
					ParametersRef: &networkv1.NetworkParametersReference{Name: gkeNetworkParamSetName, Kind: GNPKind},
				},
			},
			paramSet: paramSet,
			expectedResult: &validation{
				IsValid:      false,
				ErrorReason:  L3SecondaryMissing,
				ErrorMessage: "L3 type network requires secondary range to be specified in params",
			},
		},
		{
			name: "L3NetworkType with DeviceMode specified",
			network: &networkv1.Network{
				ObjectMeta: v1.ObjectMeta{
					Name: "test-network1",
				},
				Spec: networkv1.NetworkSpec{
					Type:          networkv1.L3NetworkType,
					ParametersRef: &networkv1.NetworkParametersReference{Name: gkeNetworkParamSetName, Kind: GNPKind},
				},
			},
			paramSet: &v1alpha1.GKENetworkParamSet{
				ObjectMeta: v1.ObjectMeta{
					Name: gkeNetworkParamSetName,
				},
				Spec: v1alpha1.GKENetworkParamSetSpec{
					VPC:           "default",
					VPCSubnet:     "test-subnet",
					PodIPv4Ranges: &v1alpha1.SecondaryRanges{RangeNames: []string{"test-secondary-range"}},
					DeviceMode:    "test-device-mode",
				},
			},
			expectedResult: &validation{
				IsValid:      false,
				ErrorReason:  L3DeviceModeExists,
				ErrorMessage: "L3 type network can't be used with a device mode specified in params",
			},
		},
		{
			name: "DeviceNetworkType with missing DeviceMode",
			network: &networkv1.Network{
				ObjectMeta: v1.ObjectMeta{
					Name: "test-network2",
				},
				Spec: networkv1.NetworkSpec{
					Type:          networkv1.DeviceNetworkType,
					ParametersRef: &networkv1.NetworkParametersReference{Name: gkeNetworkParamSetName, Kind: GNPKind},
				},
			},
			paramSet: paramSet,
			expectedResult: &validation{
				IsValid:      false,
				ErrorReason:  DeviceModeMissing,
				ErrorMessage: "Device type network requires device mode to be specified in params",
			},
		},
		{
			name: "DeviceNetworkType with PodIPv4Ranges specified",
			network: &networkv1.Network{
				ObjectMeta: v1.ObjectMeta{
					Name: "test-network2",
				},
				Spec: networkv1.NetworkSpec{
					Type:          networkv1.DeviceNetworkType,
					ParametersRef: &networkv1.NetworkParametersReference{Name: gkeNetworkParamSetName, Kind: GNPKind},
				},
			},
			paramSet: &v1alpha1.GKENetworkParamSet{
				ObjectMeta: v1.ObjectMeta{
					Name: gkeNetworkParamSetName,
				},
				Spec: v1alpha1.GKENetworkParamSetSpec{
					VPC:           "default",
					VPCSubnet:     "test-subnet",
					PodIPv4Ranges: &v1alpha1.SecondaryRanges{RangeNames: []string{"test-secondary-range"}},
					DeviceMode:    "test-device-mode",
				},
			},
			expectedResult: &validation{
				IsValid:      false,
				ErrorReason:  DeviceSecondaryExists,
				ErrorMessage: "Device type network can't be used with a secondary range specified in params",
			},
		},
		{
			name: "Valid L3NetworkType",
			network: &networkv1.Network{
				ObjectMeta: v1.ObjectMeta{
					Name: "test-network1",
				},
				Spec: networkv1.NetworkSpec{
					Type:          networkv1.L3NetworkType,
					ParametersRef: &networkv1.NetworkParametersReference{Name: gkeNetworkParamSetName, Kind: GNPKind},
				},
			},
			paramSet: &v1alpha1.GKENetworkParamSet{
				ObjectMeta: v1.ObjectMeta{
					Name: gkeNetworkParamSetName,
				},
				Spec: v1alpha1.GKENetworkParamSetSpec{
					VPC:           "default",
					VPCSubnet:     "test-subnet",
					PodIPv4Ranges: &v1alpha1.SecondaryRanges{RangeNames: []string{"test-secondary-range"}},
				},
			},
			expectedResult: &validation{
				IsValid: true,
			},
		},
		{
			name: "Valid DeviceNetworkType",
			network: &networkv1.Network{
				ObjectMeta: v1.ObjectMeta{
					Name: "test-network2",
				},
				Spec: networkv1.NetworkSpec{
					Type:          networkv1.DeviceNetworkType,
					ParametersRef: &networkv1.NetworkParametersReference{Name: gkeNetworkParamSetName, Kind: GNPKind},
				},
			},
			paramSet: &v1alpha1.GKENetworkParamSet{
				ObjectMeta: v1.ObjectMeta{
					Name: gkeNetworkParamSetName,
				},
				Spec: v1alpha1.GKENetworkParamSetSpec{
					VPC:        "default",
					VPCSubnet:  "test-subnet",
					DeviceMode: "test-device-mode",
				},
			},
			expectedResult: &validation{
				IsValid: true,
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			g := gomega.NewGomegaWithT(t)
			validationResult := crossValidateNetworkAndGnp(*test.network, test.paramSet)
			g.Expect(validationResult).To(gomega.Equal(test.expectedResult))
		})
	}
}

func TestValidateGKENetworkParamSet(t *testing.T) {
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	testVals := setupGKENetworkParamSetController()

	testVals.runGKENetworkParamSetController(ctx)

	// Create a GKENetworkParamSet
	gkeNetworkParamSetName := "test-paramset"

	subnet := &compute.Subnetwork{
		Name: "test-subnet",
		SecondaryIpRanges: []*compute.SubnetworkSecondaryRange{
			{
				IpCidrRange: "10.0.0.0/24",
				RangeName:   "test-secondary-range",
			},
		},
	}

	// Create an existing GNP with deviceMode
	existingGNP := &v1alpha1.GKENetworkParamSet{
		ObjectMeta: v1.ObjectMeta{
			Name: "existing-paramset",
		},
		Spec: v1alpha1.GKENetworkParamSetSpec{
			VPC:        "existing-vpc",
			VPCSubnet:  "existing-subnet",
			DeviceMode: "test-device-mode",
		},
	}
	_, err := testVals.networkClient.NetworkingV1alpha1().GKENetworkParamSets().Create(ctx, existingGNP, v1.CreateOptions{})
	if err != nil {
		t.Fatalf("Failed to create existing GNP: %v", err)
	}

	tests := []struct {
		name           string
		paramSet       *v1alpha1.GKENetworkParamSet
		subnet         *compute.Subnetwork
		expectedResult *validation
	}{
		{
			name: "Secondary range not found",
			paramSet: &v1alpha1.GKENetworkParamSet{
				ObjectMeta: v1.ObjectMeta{
					Name: gkeNetworkParamSetName,
				},
				Spec: v1alpha1.GKENetworkParamSetSpec{
					VPC:       "default",
					VPCSubnet: "test-subnet",
					PodIPv4Ranges: &v1alpha1.SecondaryRanges{
						RangeNames: []string{
							"nonexistent-secondary-range",
						},
					},
				},
			},
			subnet: subnet,
			expectedResult: &validation{
				IsValid:      false,
				ErrorReason:  SecondaryRangeNotFound,
				ErrorMessage: "nonexistent-secondary-range not found in test-subnet",
			},
		},
		{
			name: "DeviceMode and secondary range specified at the same time",
			paramSet: &v1alpha1.GKENetworkParamSet{
				ObjectMeta: v1.ObjectMeta{
					Name: gkeNetworkParamSetName,
				},
				Spec: v1alpha1.GKENetworkParamSetSpec{
					VPC:       "default",
					VPCSubnet: "test-subnet",
					PodIPv4Ranges: &v1alpha1.SecondaryRanges{
						RangeNames: []string{
							"test-secondary-range",
						},
					},
					DeviceMode: "test-device-mode",
				},
			},
			subnet: subnet,
			expectedResult: &validation{
				IsValid:      false,
				ErrorReason:  DeviceModeCantBeUsedWithSecondaryRange,
				ErrorMessage: "deviceMode and secondary range can not be specified at the same time",
			},
		},
		{
			name: "Valid GKENetworkParamSet",
			paramSet: &v1alpha1.GKENetworkParamSet{
				ObjectMeta: v1.ObjectMeta{
					Name: gkeNetworkParamSetName,
				},
				Spec: v1alpha1.GKENetworkParamSetSpec{
					VPC:       "default",
					VPCSubnet: "test-subnet",
					PodIPv4Ranges: &v1alpha1.SecondaryRanges{
						RangeNames: []string{
							"test-secondary-range",
						},
					},
				},
			},
			subnet: subnet,
			expectedResult: &validation{
				IsValid: true,
			},
		},
		{
			name: "GNP with deviceMode and referencing VPC is referenced in any other existing GNP",
			paramSet: &v1alpha1.GKENetworkParamSet{
				ObjectMeta: v1.ObjectMeta{
					Name: gkeNetworkParamSetName,
				},
				Spec: v1alpha1.GKENetworkParamSetSpec{
					VPC:        "existing-vpc",
					VPCSubnet:  "test-subnet",
					DeviceMode: "test-device-mode",
				},
			},
			subnet: subnet,
			expectedResult: &validation{
				IsValid:      false,
				ErrorReason:  DeviceModeVPCAlreadyInUse,
				ErrorMessage: "GNP with deviceMode can't reference a VPC already in use. VPC 'existing-vpc' is already in use by 'existing-paramset'",
			},
		},
		{
			name: "GNP with deviceMode and the referencing VPC is the default VPC",
			paramSet: &v1alpha1.GKENetworkParamSet{
				ObjectMeta: v1.ObjectMeta{
					Name: gkeNetworkParamSetName,
				},
				Spec: v1alpha1.GKENetworkParamSetSpec{
					VPC:        testVals.clusterValues.NetworkName,
					VPCSubnet:  "test-subnet",
					DeviceMode: "test-device-mode",
				},
			},
			subnet: subnet,
			expectedResult: &validation{
				IsValid:      false,
				ErrorReason:  DeviceModeCantUseDefaultVPC,
				ErrorMessage: "GNP with deviceMode can't reference the default VPC",
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			g := gomega.NewGomegaWithT(t)
			validationResult, err := testVals.controller.validateGKENetworkParamSet(ctx, test.paramSet, test.subnet)
			if err != nil {
				t.Fatal("validateGKENetworkParamSet should not error as no actual connection to the cluster is being made")
			}
			g.Expect(validationResult).To(gomega.Equal(test.expectedResult))
		})
	}
}
