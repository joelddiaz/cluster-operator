/*
Copyright 2017 The Kubernetes Authors.

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

package remotemachineset

import (
	"testing"

	cov1 "github.com/openshift/cluster-operator/pkg/apis/clusteroperator/v1alpha1"
	"github.com/openshift/cluster-operator/pkg/controller"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	clientgofake "k8s.io/client-go/kubernetes/fake"
	clientgotesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/cache"

	clusteropclientfake "github.com/openshift/cluster-operator/pkg/client/clientset_generated/clientset/fake"
	clusteropinformers "github.com/openshift/cluster-operator/pkg/client/informers_generated/externalversions"

	clusterapiv1 "sigs.k8s.io/cluster-api/pkg/apis/cluster/v1alpha1"
	clusterapiclient "sigs.k8s.io/cluster-api/pkg/client/clientset_generated/clientset"
	clusterapiclientfake "sigs.k8s.io/cluster-api/pkg/client/clientset_generated/clientset/fake"
	clusterapiinformers "sigs.k8s.io/cluster-api/pkg/client/informers_generated/externalversions"

	log "github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
)

const (
	testRegion           = "us-east-1"
	testClusterVerUID    = types.UID("test-cluster-version")
	testClusterVerName   = "v3-9"
	testClusterVerNS     = "cluster-operator"
	testClusterUUID      = types.UID("test-cluster-uuid")
	testClusterName      = "testcluster"
	testClusterNamespace = "testsyncns"
)

func init() {
	log.SetLevel(log.DebugLevel)
}

func newTestRemoteClusterAPIClientWithObjects(objects []kruntime.Object) *clusterapiclientfake.Clientset {
	remoteClient := clusterapiclientfake.NewSimpleClientset(objects...)
	return remoteClient
}

func newTestRemoteClusterAPIClient() (
	cache.Store,
	cache.Store,
	*clusterapiclientfake.Clientset,
) {
	remoteClient := &clusterapiclientfake.Clientset{}
	informers := clusterapiinformers.NewSharedInformerFactory(remoteClient, 0)
	return informers.Cluster().V1alpha1().Clusters().Informer().GetStore(),
		informers.Cluster().V1alpha1().MachineSets().Informer().GetStore(),
		remoteClient
}

type testContext struct {
	controller             *Controller
	clusterDeploymentStore cache.Store
	clusterVersionStore    cache.Store
	clusterStore           cache.Store
	clusteropclient        *clusteropclientfake.Clientset
	capiclient             *clusterapiclientfake.Clientset
	kubeclient             *clientgofake.Clientset
}

// Sets up all of the mocks, informers, cache, etc for tests.
func setupTest() *testContext {
	kubeClient := &clientgofake.Clientset{}
	clusteropClient := &clusteropclientfake.Clientset{}
	capiClient := &clusterapiclientfake.Clientset{}

	clusteropInformers := clusteropinformers.NewSharedInformerFactory(clusteropClient, 0)
	clusterapiInformers := clusterapiinformers.NewSharedInformerFactory(capiClient, 0)

	ctx := &testContext{
		controller: NewController(
			clusteropInformers.Clusteroperator().V1alpha1().ClusterDeployments(),
			clusteropInformers.Clusteroperator().V1alpha1().ClusterVersions(),
			clusterapiInformers.Cluster().V1alpha1().Clusters(),
			kubeClient,
			clusteropClient,
		),
		clusterDeploymentStore: clusteropInformers.Clusteroperator().V1alpha1().ClusterDeployments().Informer().GetStore(),
		clusterVersionStore:    clusteropInformers.Clusteroperator().V1alpha1().ClusterVersions().Informer().GetStore(),
		clusterStore:           clusterapiInformers.Cluster().V1alpha1().Clusters().Informer().GetStore(),
		clusteropclient:        clusteropClient,
		capiclient:             capiClient,
		kubeclient:             kubeClient,
	}
	return ctx
}

func TestClusterSyncing(t *testing.T) {

	cases := []struct {
		name              string
		controlPlaneReady bool
		errorExpected     string
		expectedActions   []expectedRemoteAction
		unexpectedActions []expectedRemoteAction
		clusterDeployment *cov1.ClusterDeployment
		remoteClusters    []clusterapiv1.Cluster
	}{
		{
			name: "test cluster create",
			expectedActions: []expectedRemoteAction{
				{
					namespace: remoteClusterAPINamespace,
					verb:      "create",
					gvr:       clusterapiv1.SchemeGroupVersion.WithResource("cluster"),
				},
			},
			clusterDeployment: newTestClusterDeployment(true),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// :ASSEMBLE:
			// Setup the mocks, informers, etc
			ctx := setupTest()

			// We need to be able to see what the controller is logging to validate the process happened.
			tLog := ctx.controller.logger

			// set up a fake cov1.ClusterVersion so that when the controller runs, the object is available.
			cv := newClusterVer(testClusterVerNS, testClusterVerName, testClusterVerUID)
			err := ctx.clusterVersionStore.Add(cv)
			if err != nil {
				// Tell the test runner that we've failed. This ends the test.
				t.Fatalf("Error storing clusterversion object: %v", err)
			}

			// set up remote cluster objects (generic list basically)
			existingObjects := []kruntime.Object{}

			// If the tc data contains definitions for remoteClusters, then add them to existingObjects.
			for i := range tc.remoteClusters {
				existingObjects = append(existingObjects, &tc.remoteClusters[i]) // For implicit conversion, must be done this way.
			}

			// Using our list of kruntime objects, create a clusterapi client mock
			remoteClusterAPIClient := newTestRemoteClusterAPIClientWithObjects(existingObjects)

			// Override the remotemachineset controller's buildRemoteClient function to use our mock instead.
			ctx.controller.buildRemoteClient = func(*cov1.ClusterDeployment) (clusterapiclient.Interface, error) {
				return remoteClusterAPIClient, nil
			}

			// create cluster api cluster object
			capiCluster, err := newCapiCluster(tc.clusterDeployment)
			if err != nil {
				t.Fatalf("failed to get providerstaus from clusterdeployment: %v", err)
			}

			// Mock that the informer's cache has a cluster api cluster object in it.
			ctx.clusterStore.Add(capiCluster)

			// Mock that the informer's cache has a cluster opertaor cluster deployment in it.
			ctx.clusterDeploymentStore.Add(tc.clusterDeployment)

			// :ACT:
			err = ctx.controller.syncClusterDeployment(getKey(tc.clusterDeployment, t))

			// :ASSERT:
			if tc.errorExpected != "" {
				if assert.Error(t, err) {
					assert.Contains(t, err.Error(), tc.errorExpected)
				}
			} else {
				assert.NoError(t, err)
			}

			validateExpectedActions(t, tLog, remoteClusterAPIClient.Actions(), tc.expectedActions)
			validateUnexpectedActions(t, tLog, remoteClusterAPIClient.Actions(), tc.unexpectedActions)
		})
	}
}

func TestMachineSetSyncing(t *testing.T) {
	cases := []struct {
		name              string
		controlPlaneReady bool
		errorExpected     string
		expectedActions   []expectedRemoteAction
		unexpectedActions []expectedRemoteAction
		clusterDeployment *cov1.ClusterDeployment
		remoteMachineSets []clusterapiv1.MachineSet
	}{
		{
			name: "no-op when control plane not yet ready",
			unexpectedActions: []expectedRemoteAction{
				{
					namespace: remoteClusterAPINamespace,
					verb:      "create",
					gvr:       clusterapiv1.SchemeGroupVersion.WithResource("machinesets"),
				},
			},
			clusterDeployment: newTestClusterDeployment(false),
		},
		{
			name: "creates initial machine set",
			expectedActions: []expectedRemoteAction{
				{
					namespace: remoteClusterAPINamespace,
					verb:      "create",
					gvr:       clusterapiv1.SchemeGroupVersion.WithResource("machinesets"),
				},
			},
			clusterDeployment: newTestClusterWithCompute(true),
		},
		{
			name: "no-op when machineset already on remote",
			unexpectedActions: []expectedRemoteAction{
				{
					namespace: remoteClusterAPINamespace,
					verb:      "create",
					gvr:       clusterapiv1.SchemeGroupVersion.WithResource("machinesets"),
				},
			},
			clusterDeployment: newTestClusterWithCompute(true),
			remoteMachineSets: []clusterapiv1.MachineSet{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "compute",
						Namespace: remoteClusterAPINamespace,
					},
					Spec: clusterapiv1.MachineSetSpec{
						Replicas: func() *int32 { x := int32(1); return &x }(),
					},
				},
			},
		},
		{
			name: "update when remote machineset is outdated",
			expectedActions: []expectedRemoteAction{
				{
					namespace: remoteClusterAPINamespace,
					verb:      "update",
					gvr:       clusterapiv1.SchemeGroupVersion.WithResource("machinesets"),
				},
			},
			clusterDeployment: newTestClusterWithCompute(true),
			remoteMachineSets: []clusterapiv1.MachineSet{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "compute",
						Namespace: remoteClusterAPINamespace,
					},
					Spec: clusterapiv1.MachineSetSpec{
						Replicas: func() *int32 { x := int32(2); return &x }(),
					},
				},
			},
		},
		{
			name: "create when additional machineset appears",
			expectedActions: []expectedRemoteAction{
				{
					namespace: remoteClusterAPINamespace,
					verb:      "create",
					gvr:       clusterapiv1.SchemeGroupVersion.WithResource("machinesets"),
				},
			},
			clusterDeployment: func() *cov1.ClusterDeployment {
				cluster := newTestClusterWithCompute(true)
				cluster = addMachineSetToCluster(cluster, "compute2", cov1.NodeTypeCompute, false, 1)
				return cluster
			}(),
			remoteMachineSets: []clusterapiv1.MachineSet{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "compute",
						Namespace: remoteClusterAPINamespace,
					},
					Spec: clusterapiv1.MachineSetSpec{
						Replicas: func() *int32 { x := int32(2); return &x }(),
					},
				},
			},
		},
		{
			name: "delete extra remote machineset",
			expectedActions: []expectedRemoteAction{
				{
					namespace: remoteClusterAPINamespace,
					verb:      "delete",
					gvr:       clusterapiv1.SchemeGroupVersion.WithResource("machinesets"),
				},
			},
			clusterDeployment: newTestClusterWithCompute(true),
			remoteMachineSets: []clusterapiv1.MachineSet{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "compute",
						Namespace: remoteClusterAPINamespace,
					},
					Spec: clusterapiv1.MachineSetSpec{
						Replicas: func() *int32 { x := int32(1); return &x }(),
					},
				},
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "compute2",
						Namespace: remoteClusterAPINamespace,
					},
					Spec: clusterapiv1.MachineSetSpec{
						Replicas: func() *int32 { x := int32(1); return &x }(),
					},
				},
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := setupTest()
			tLog := ctx.controller.logger

			// set up clusterversions
			cv := newClusterVer(testClusterVerNS, testClusterVerName, testClusterVerUID)
			err := ctx.clusterVersionStore.Add(cv)
			if err != nil {
				t.Fatalf("Error storing clusterversion object: %v", err)
			}

			// set up remote cluster objects
			existingObjects := []kruntime.Object{}

			for i := range tc.remoteMachineSets {
				existingObjects = append(existingObjects, &tc.remoteMachineSets[i])
			}
			remoteClusterAPIClient := newTestRemoteClusterAPIClientWithObjects(existingObjects)

			ctx.controller.buildRemoteClient = func(*cov1.ClusterDeployment) (clusterapiclient.Interface, error) {
				return remoteClusterAPIClient, nil
			}

			// create cluster api cluster object
			capiCluster, err := newCapiCluster(tc.clusterDeployment)
			if err != nil {
				t.Fatalf("failed to get providerstaus from clusterdeployment: %v", err)
			}
			ctx.clusterStore.Add(capiCluster)

			ctx.clusterDeploymentStore.Add(tc.clusterDeployment)
			err = ctx.controller.syncClusterDeployment(getKey(tc.clusterDeployment, t))

			if tc.errorExpected != "" {
				if assert.Error(t, err) {
					assert.Contains(t, err.Error(), tc.errorExpected)
				}
			} else {
				assert.NoError(t, err)
			}

			validateExpectedActions(t, tLog, remoteClusterAPIClient.Actions(), tc.expectedActions)
			validateUnexpectedActions(t, tLog, remoteClusterAPIClient.Actions(), tc.unexpectedActions)
		})
	}
}

func validateExpectedActions(t *testing.T, tLog log.FieldLogger, actions []clientgotesting.Action, expectedActions []expectedRemoteAction) {
	anyMissing := false
	for _, ea := range expectedActions {
		found := false
		for _, a := range actions {
			if a.GetNamespace() == ea.namespace &&
				a.GetResource() == ea.gvr &&
				a.GetVerb() == ea.verb {
				found = true
			}
		}
		if !found {
			anyMissing = true
		}
		assert.True(t, found, "unable to find expected remote client action: %v", ea)
	}
	if anyMissing {
		tLog.Warnf("remote client actions found: %v", actions)
	}
}

func validateUnexpectedActions(t *testing.T, tLog log.FieldLogger, actions []clientgotesting.Action, unexpectedActions []expectedRemoteAction) {
	for _, uea := range unexpectedActions {
		for _, a := range actions {
			if a.GetNamespace() == uea.namespace &&
				a.GetResource() == uea.gvr &&
				a.GetVerb() == uea.verb {
				t.Errorf("found unexpected remote client action: %v", a)
			}
		}
	}
}

type expectedRemoteAction struct {
	namespace string
	verb      string
	gvr       schema.GroupVersionResource
}

// alwaysReady is a function that can be used as a sync function that will
// always indicate that the lister has been synced.
var alwaysReady = func() bool { return true }

func getKey(cluster *cov1.ClusterDeployment, t *testing.T) string {
	key, err := controller.KeyFunc(cluster)
	if err != nil {
		t.Errorf("Unexpected error getting key for cluster %v: %v", cluster.Name, err)
		return ""
	}
	return key
}

func newCapiCluster(clusterDeployment *cov1.ClusterDeployment) (*clusterapiv1.Cluster, error) {
	cluster := &clusterapiv1.Cluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:      clusterDeployment.Name,
			Namespace: clusterDeployment.Namespace,
		},
		Spec: clusterapiv1.ClusterSpec{
			ClusterNetwork: clusterapiv1.ClusterNetworkingConfig{
				Services: clusterapiv1.NetworkRanges{
					CIDRBlocks: []string{"172.30.0.0/16"},
				},
				ServiceDomain: "svc.clsuter.local",
				Pods: clusterapiv1.NetworkRanges{
					CIDRBlocks: []string{"10.128.0.0/14"},
				},
			},
		},
	}

	providerStatus, err := controller.ClusterAPIProviderStatusFromClusterStatus(&clusterDeployment.Status)
	if err != nil {
		return nil, err
	}
	cluster.Status.ProviderStatus = providerStatus

	return cluster, nil
}

// newClusterVer will create an actual ClusterVersion for the given reference.
// Used when we want to make sure a version ref specified on a Cluster exists in the store.
func newClusterVer(namespace, name string, uid types.UID) *cov1.ClusterVersion {
	cv := &cov1.ClusterVersion{
		ObjectMeta: metav1.ObjectMeta{
			UID:       uid,
			Name:      name,
			Namespace: namespace,
		},
		Spec: cov1.ClusterVersionSpec{
			ImageFormat: "openshift/origin-${component}:${version}",
			VMImages: cov1.VMImages{
				AWSImages: &cov1.AWSVMImages{
					RegionAMIs: []cov1.AWSRegionAMIs{
						{
							Region: testRegion,
							AMI:    "computeAMI_ID",
						},
					},
				},
			},
		},
	}
	return cv
}

func newTestClusterWithCompute(controlPlaneReady bool) *cov1.ClusterDeployment {
	cluster := newTestClusterDeployment(controlPlaneReady)
	cluster = addMachineSetToCluster(cluster, "compute", cov1.NodeTypeCompute, false, 1)
	return cluster
}

func newTestClusterDeployment(controlPlaneReady bool) *cov1.ClusterDeployment {
	clusterDeployment := &cov1.ClusterDeployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testClusterName,
			Namespace: testClusterNamespace,
			UID:       testClusterUUID,
		},
		Spec: cov1.ClusterDeploymentSpec{
			Hardware: cov1.ClusterHardwareSpec{
				AWS: &cov1.AWSClusterSpec{
					Region: testRegion,
				},
			},
			MachineSets: []cov1.ClusterMachineSet{
				cov1.ClusterMachineSet{
					ShortName: "master",
					MachineSetConfig: cov1.MachineSetConfig{
						Infra:    false,
						NodeType: cov1.NodeTypeMaster,
						Size:     1,
					},
				},
			},
			ClusterVersionRef: cov1.ClusterVersionReference{
				Name:      testClusterVerName,
				Namespace: testClusterVerNS,
			},
		},
		Status: cov1.ClusterDeploymentStatus{
			ClusterAPIInstalled:   controlPlaneReady,
			ControlPlaneInstalled: controlPlaneReady,
		},
	}
	return clusterDeployment
}

func addMachineSetToCluster(cluster *cov1.ClusterDeployment, shortName string, nodeType cov1.NodeType, infra bool, size int) *cov1.ClusterDeployment {
	cluster.Spec.MachineSets = append(cluster.Spec.MachineSets,
		cov1.ClusterMachineSet{
			ShortName: shortName,
			MachineSetConfig: cov1.MachineSetConfig{
				NodeType: nodeType,
				Infra:    infra,
				Size:     size,
			},
		})

	return cluster
}
