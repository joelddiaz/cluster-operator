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

package syncclusterdeployment

import (
	"testing"

	cov1 "github.com/openshift/cluster-operator/pkg/apis/clusteroperator/v1alpha1"
	coclient "github.com/openshift/cluster-operator/pkg/client/clientset_generated/clientset/fake"
	informers "github.com/openshift/cluster-operator/pkg/client/informers_generated/externalversions"
	"github.com/openshift/cluster-operator/pkg/controller"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	clientgofake "k8s.io/client-go/kubernetes/fake"
	clientgotesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/cache"

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

// newTestController creates a test Controller with fake clients and informers.
func newTestController() (
	*Controller,
	cache.Store, // cluster version store
	cache.Store, // clusters store
	*clientgofake.Clientset,
	*coclient.Clientset,
) {
	kubeClient := &clientgofake.Clientset{}
	clusterOperatorClient := &coclient.Clientset{}
	informers := informers.NewSharedInformerFactory(clusterOperatorClient, 0)

	controller := NewController(
		informers.Clusteroperator().V1alpha1().ClusterDeployments(),
		kubeClient,
		clusterOperatorClient,
	)

	controller.clustersSynced = alwaysReady

	return controller,
		informers.Clusteroperator().V1alpha1().ClusterVersions().Informer().GetStore(),
		informers.Clusteroperator().V1alpha1().ClusterDeployments().Informer().GetStore(),
		kubeClient,
		clusterOperatorClient
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

func TestMachineSetSyncing(t *testing.T) {
	cases := []struct {
		name              string
		controlPlaneReady bool
		errorExpected     string
		expectedActions   []expectedRemoteAction
		unexpectedActions []expectedRemoteAction
		cluster           *cov1.ClusterDeployment
		remoteMachineSets []clusterapiv1.MachineSet
	}{
		{
			name:              "no-op when control plane not yet ready",
			controlPlaneReady: false,
			unexpectedActions: []expectedRemoteAction{
				{
					namespace: remoteClusterAPINamespace,
					verb:      "create",
					gvr:       clusterapiv1.SchemeGroupVersion.WithResource("machinesets"),
				},
			},
			cluster: newTestCluster(),
		},
		{
			name:              "creates initial machine set",
			controlPlaneReady: true,
			expectedActions: []expectedRemoteAction{
				{
					namespace: remoteClusterAPINamespace,
					verb:      "create",
					gvr:       clusterapiv1.SchemeGroupVersion.WithResource("machinesets"),
				},
			},
			cluster: newTestClusterWithCompute(),
		},
		{
			name:              "no-op when machineset already on remote",
			controlPlaneReady: true,
			unexpectedActions: []expectedRemoteAction{
				{
					namespace: remoteClusterAPINamespace,
					verb:      "create",
					gvr:       clusterapiv1.SchemeGroupVersion.WithResource("machinesets"),
				},
			},
			cluster: newTestClusterWithCompute(),
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
			name:              "update when remote machineset is outdated",
			controlPlaneReady: true,
			expectedActions: []expectedRemoteAction{
				{
					namespace: remoteClusterAPINamespace,
					verb:      "update",
					gvr:       clusterapiv1.SchemeGroupVersion.WithResource("machinesets"),
				},
			},
			cluster: newTestClusterWithCompute(),
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
			name:              "create when additional machineset appears",
			controlPlaneReady: true,
			expectedActions: []expectedRemoteAction{
				{
					namespace: remoteClusterAPINamespace,
					verb:      "create",
					gvr:       clusterapiv1.SchemeGroupVersion.WithResource("machinesets"),
				},
			},
			cluster: func() *cov1.ClusterDeployment {
				cluster := newTestClusterWithCompute()
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
			name:              "delete extra remote machineset",
			controlPlaneReady: true,
			expectedActions: []expectedRemoteAction{
				{
					namespace: remoteClusterAPINamespace,
					verb:      "delete",
					gvr:       clusterapiv1.SchemeGroupVersion.WithResource("machinesets"),
				},
			},
			cluster: newTestClusterWithCompute(),
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

			c, cvStore, cStore, _, _ := newTestController()
			cv := newClusterVer(testClusterVerNS, testClusterVerName, testClusterVerUID)
			cvStore.Add(cv)
			tLog := c.logger

			existingObjects := []kruntime.Object{}

			for i := range tc.remoteMachineSets {
				existingObjects = append(existingObjects, &tc.remoteMachineSets[i])
			}
			remoteClusterAPIClient := newTestRemoteClusterAPIClientWithObjects(existingObjects)

			if tc.controlPlaneReady {
				tc.cluster.Status.ControlPlaneInstalled = true
				tc.cluster.Status.ClusterAPIInstalled = true
			}
			cStore.Add(tc.cluster)

			c.BuildRemoteClient = func(*cov1.ClusterDeployment) (clusterapiclient.Interface, error) {
				return remoteClusterAPIClient, nil
			}

			err := c.syncClusterDeployment(getKey(tc.cluster, t))
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
		assert.True(t, found, "unable to find expected remote client action: %v", ea)
	}
	if anyMissing {
		tLog.Warnf("remote client actions found: %v", actions)
	}
}

func validateUnexpectedActions(t *testing.T, tLog log.FieldLogger, actions []clientgotesting.Action, unexpectedActions []expectedRemoteAction) {
	for _, ea := range unexpectedActions {
		for _, a := range actions {
			if a.GetNamespace() == ea.namespace &&
				a.GetResource() == ea.gvr &&
				a.GetVerb() == ea.verb {
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

func newAPIReadyTestCluster() *cov1.ClusterDeployment {
	cluster := newTestCluster()
	cluster.Status.ClusterAPIInstalled = true
	cluster.Status.ControlPlaneInstalled = true
	return cluster
}

func newTestClusterWithCompute() *cov1.ClusterDeployment {
	cluster := newTestCluster()
	cluster = addMachineSetToCluster(cluster, "compute", cov1.NodeTypeCompute, false, 1)
	return cluster
}

func newTestCluster() *cov1.ClusterDeployment {
	cluster := &cov1.ClusterDeployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testClusterName,
			Namespace: testClusterNamespace,
			UID:       testClusterUUID,
		},
		Spec: cov1.ClusterDeploymentSpec{
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
		},
	}
	return cluster
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
