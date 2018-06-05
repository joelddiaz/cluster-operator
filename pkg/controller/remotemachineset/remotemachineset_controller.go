/*
Copyright 2018 The Kubernetes Authors.

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
	"fmt"
	"time"

	"github.com/golang/glog"
	log "github.com/sirupsen/logrus"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	kubeclientset "k8s.io/client-go/kubernetes"
	v1core "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/workqueue"

	cov1 "github.com/openshift/cluster-operator/pkg/apis/clusteroperator/v1alpha1"
	coclient "github.com/openshift/cluster-operator/pkg/client/clientset_generated/clientset"
	informers "github.com/openshift/cluster-operator/pkg/client/informers_generated/externalversions/clusteroperator/v1alpha1"
	lister "github.com/openshift/cluster-operator/pkg/client/listers_generated/clusteroperator/v1alpha1"
	"github.com/openshift/cluster-operator/pkg/controller"
	"github.com/openshift/cluster-operator/pkg/kubernetes/pkg/util/metrics"

	clusterapiv1 "sigs.k8s.io/cluster-api/pkg/apis/cluster/v1alpha1"
	clusterapiclient "sigs.k8s.io/cluster-api/pkg/client/clientset_generated/clientset"
	clusterapiinformers "sigs.k8s.io/cluster-api/pkg/client/informers_generated/externalversions/cluster/v1alpha1"
	clusterapilister "sigs.k8s.io/cluster-api/pkg/client/listers_generated/cluster/v1alpha1"
)

const (
	// maxRetries is the number of times a service will be retried before it is dropped out of the queue.
	// With the current rate-limiter in use (5ms*2^(maxRetries-1)) the following numbers represent the
	// sequence of delays between successive queuings of a machineset.
	//
	// 5ms, 10ms, 20ms, 40ms, 80ms, 160ms, 320ms, 640ms, 1.3s, 2.6s, 5.1s, 10.2s, 20.4s, 41s, 82s
	maxRetries = 15

	controllerLogName = "remotemachineset"

	remoteClusterAPINamespace      = "kube-cluster"
	errorMsgClusterAPINotInstalled = "cannot sync until cluster API is installed and ready"
)

// NewController returns a new *Controller.
func NewController(
	clusterDeploymentInformer informers.ClusterDeploymentInformer,
	clusterVersionInformer informers.ClusterVersionInformer,
	clusterInformer clusterapiinformers.ClusterInformer,
	kubeClient kubeclientset.Interface,
	clusteroperatorClient coclient.Interface,
) *Controller {
	eventBroadcaster := record.NewBroadcaster()
	eventBroadcaster.StartLogging(glog.Infof)
	// TODO: remove the wrapper when every clients have moved to use the clientset.
	eventBroadcaster.StartRecordingToSink(&v1core.EventSinkImpl{Interface: v1core.New(kubeClient.CoreV1().RESTClient()).Events("")})

	if kubeClient != nil && kubeClient.CoreV1().RESTClient().GetRateLimiter() != nil {
		metrics.RegisterMetricAndTrackRateLimiterUsage("clusteroperator_remotemachineset_controller", kubeClient.CoreV1().RESTClient().GetRateLimiter())
	}

	logger := log.WithField("controller", controllerLogName)

	c := &Controller{
		client:     clusteroperatorClient,
		kubeClient: kubeClient,
		queue:      workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), "remotemachineset"),
		logger:     logger,
	}

	clusterDeploymentInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    c.addClusterDeployment,
		UpdateFunc: c.updateClusterDeployment,
	})
	c.clusterDeploymentLister = clusterDeploymentInformer.Lister()
	c.clusterDeploymentSynced = clusterDeploymentInformer.Informer().HasSynced

	c.clusterVersionInformer = clusterVersionInformer.Lister()

	c.clusterInformer = clusterInformer.Lister()

	c.syncHandler = c.syncClusterDeployment
	c.enqueueClusterDeployment = c.enqueue
	c.buildRemoteClient = c.buildRemoteClusterClient

	return c
}

// Controller manages syncing cluster and machineset objects with a
// remote cluster API
type Controller struct {
	client     coclient.Interface
	kubeClient kubeclientset.Interface

	// To allow injection of syncClusterDeployment for testing.
	syncHandler func(key string) error

	// Used for unit testing
	enqueueClusterDeployment func(cluster *cov1.ClusterDeployment)

	buildRemoteClient func(*cov1.ClusterDeployment) (clusterapiclient.Interface, error)

	// clusterDeploymentLister is able to list/get clusters and is populated
	// by the shared informer passed to NewController.
	clusterDeploymentLister lister.ClusterDeploymentLister
	// clusterDeploymentSynced returns true if the cluster shared informer
	// has been synced at least once. Added as a member to the
	// struct to allow injection for testing.
	clusterDeploymentSynced cache.InformerSynced

	// list/get clusterversion objects
	clusterVersionInformer lister.ClusterVersionLister

	// list/get clusterapi cluster objects
	clusterInformer clusterapilister.ClusterLister

	// ClusterDeployments that need to be synced
	queue workqueue.RateLimitingInterface

	logger log.FieldLogger
}

func (c *Controller) addClusterDeployment(obj interface{}) {
	cluster := obj.(*cov1.ClusterDeployment)
	c.logger.Debugf("remotemachineset: adding cluster %v", cluster.Name)
	c.enqueueClusterDeployment(cluster)
}

func (c *Controller) updateClusterDeployment(old, cur interface{}) {
	cluster := cur.(*cov1.ClusterDeployment)
	c.logger.Debugf("remotemachineset: updating cluster %v", cluster.Name)
	c.enqueueClusterDeployment(cluster)
}

// Run runs c; will not return until stopCh is closed. workers determines how
// many machine sets will be handled in parallel.
func (c *Controller) Run(workers int, stopCh <-chan struct{}) {
	defer utilruntime.HandleCrash()
	defer c.queue.ShutDown()

	c.logger.Infof("Starting remotemachineset controller")
	defer c.logger.Infof("Shutting down remotemachineset controller")

	if !controller.WaitForCacheSync("remotemachineset", stopCh, c.clusterDeploymentSynced) {
		c.logger.Errorf("could not sync caches for remotemachineset controller")
		return
	}

	for i := 0; i < workers; i++ {
		go wait.Until(c.worker, time.Second, stopCh)
	}

	<-stopCh
}

func (c *Controller) enqueue(clusterDeployment *cov1.ClusterDeployment) {
	key, err := controller.KeyFunc(clusterDeployment)
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("couldn't get key for object %#v: %v", clusterDeployment, err))
		return
	}
	c.logger.Debugf("XXX adding cluster %v", key)
	c.queue.Add(key)
}

// worker runs a worker thread that just dequeues items, processes them, and marks them done.
// It enforces that the syncHandler is never invoked concurrently with the same key.
func (c *Controller) worker() {
	for c.processNextWorkItem() {
	}
}

func (c *Controller) processNextWorkItem() bool {
	key, quit := c.queue.Get()
	if quit {
		return false
	}
	defer c.queue.Done(key)

	err := c.syncHandler(key.(string))
	c.handleErr(err, key.(string))

	return true
}

func (c *Controller) handleErr(err error, key string) {
	if err == nil {
		c.queue.Forget(key)
		return
	}

	logger := c.logger.WithField("clusterdeployment", key)

	if c.queue.NumRequeues(key) < maxRetries {
		logger.Infof("error syncing cluster deployment: %v", err)
		c.queue.AddRateLimited(key)
		return
	}

	utilruntime.HandleError(err)
	logger.Infof("dropping cluster deployment out of the queue: %v", err)
	c.queue.Forget(key)
}

func isMasterMachineSet(machineSet *cov1.ClusterMachineSet) bool {
	return machineSet.NodeType == cov1.NodeTypeMaster
}

func (c *Controller) buildRemoteClusterClient(clusterDeployment *cov1.ClusterDeployment) (clusterapiclient.Interface, error) {
	// Load the kubeconfig secret for communicating with the remote cluster:

	// Step 1: Get the secret ref from the cluster
	//
	// TODO: The cluster controller should watch secrets until it
	// sees a kubeconfig secret named after the cluster and then
	// update cluster status with a reference to that
	// secret. Until then we'll look for a
	// "clustername-kubeconfig" secret.
	//
	// clusterConfig := cluster.Status.AdminKubeconfig
	// if clusterConfig == nil {
	// 	return nil, fmt.Errorf("unable to retrieve AdminKubeConfig from cluster status")
	// }
	secretName := clusterDeployment.Name + "-kubeconfig"

	// Step 2: Retrieve secret
	secret, err := c.kubeClient.CoreV1().Secrets(clusterDeployment.Namespace).Get(secretName, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	secretData := secret.Data["kubeconfig"]

	// Step 3: Generate config from secret data
	config, err := clientcmd.Load(secretData)
	if err != nil {
		return nil, err
	}

	kubeConfig := clientcmd.NewDefaultClientConfig(*config, &clientcmd.ConfigOverrides{})
	restConfig, err := kubeConfig.ClientConfig()
	if err != nil {
		return nil, err
	}

	// Step 4: Return client for remote cluster
	return clusterapiclient.NewForConfig(restConfig)
}

// syncClusterDeployment will sync the clusterdeployment with the given key.
// This function is not meant to be invoked concurrently with the same key.
func (c *Controller) syncClusterDeployment(key string) error {
	startTime := time.Now()
	c.logger.WithField("key", key).Debug("syncing clusterdeployment")
	defer func() {
		c.logger.WithFields(log.Fields{
			"key":      key,
			"duration": time.Now().Sub(startTime),
		}).Debug("finished syncing clusterdeployment")
	}()

	namespace, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		return err
	}

	clusterDeployment, err := c.clusterDeploymentLister.ClusterDeployments(namespace).Get(name)
	if err != nil {
		c.logger.Errorf("Error fetching clusterdeployment %v/%v", namespace, name)
		return err
	}

	clusterapiCluster, err := c.clusterInformer.Clusters(clusterDeployment.Namespace).Get(clusterDeployment.Name)
	if err != nil {
		return fmt.Errorf("error retrieving cluster object: %v", err)
	}

	err = c.syncMachineSets(clusterDeployment, clusterapiCluster)

	return err
}

func (c *Controller) syncClusterSpec(clusterDeployment *cov1.ClusterDeployment, clusterapiCluster *clusterapiv1.Cluster) error {

	return nil
}

// makes sure the remote cluster's MachineSets match the provided ClusterDeployment.Spec.MachineSets[]
func (c *Controller) syncMachineSets(clusterDeployment *cov1.ClusterDeployment, clusterapiCluster *clusterapiv1.Cluster) error {
	clusterDeploymentStatus, err := controller.ClusterStatusFromClusterAPI(clusterapiCluster)
	if err != nil {
		return fmt.Errorf("error fetching cluster status: %v", err)
	}

	if !clusterDeploymentStatus.ClusterAPIInstalled {
		c.logger.Debugf(errorMsgClusterAPINotInstalled)
		return nil
	}

	machineSetsToDelete := []*clusterapiv1.MachineSet{}
	machineSetsToCreate := []*clusterapiv1.MachineSet{}
	machineSetsToUpdate := []*clusterapiv1.MachineSet{}

	remoteClusterAPIClient, err := c.buildRemoteClient(clusterDeployment)
	if err != nil {
		return err
	}
	c.logger.Debugf("got remote cluster client: %+v", remoteClusterAPIClient)
	remoteMachineSets, err := remoteClusterAPIClient.ClusterV1alpha1().MachineSets(remoteClusterAPINamespace).List(metav1.ListOptions{})

	coVersionNamespace, coVersionName := clusterDeployment.Spec.ClusterVersionRef.Namespace, clusterDeployment.Spec.ClusterVersionRef.Name
	coClusterVersion, err := c.clusterVersionInformer.ClusterVersions(coVersionNamespace).Get(coVersionName)
	if err != nil {
		return fmt.Errorf("cannot retrieve cluster version %s/%s: %v", coVersionNamespace, coVersionName, err)
	}

	clusterDeploymentComputeMachineSets, err := computeClusterAPIMachineSetsFromClusterDeployment(clusterDeployment, coClusterVersion)
	if err != nil {
		return err
	}

	// find items that need updating/creating
	for _, ms := range clusterDeploymentComputeMachineSets {
		found := false
		for _, rMS := range remoteMachineSets.Items {
			if ms.Name == rMS.Name {
				found = true
				// TODO what other fields do we want to check for?
				if *rMS.Spec.Replicas != *ms.Spec.Replicas {
					machineSetsToUpdate = append(machineSetsToUpdate, ms)
				}
				break
			}
		}

		if !found {
			machineSetsToCreate = append(machineSetsToCreate, ms)
		}

	}

	// find items that need deleting
	for _, rMS := range remoteMachineSets.Items {
		found := false
		for _, ms := range clusterDeploymentComputeMachineSets {
			if rMS.Name == ms.Name {
				found = true
				break
			}
		}
		if !found {
			machineSetsToDelete = append(machineSetsToDelete, &rMS)
		}
	}

	c.logger.Debugf("Creating %d machineset(s)", len(machineSetsToCreate))
	c.logger.Debugf("Updating %d machineset(s)", len(machineSetsToUpdate))
	c.logger.Debugf("Deleting %d machineset(s)", len(machineSetsToDelete))

	for _, ms := range machineSetsToCreate {
		c.logger.Debugf("creating ms %v", ms.Name)
		_, err = remoteClusterAPIClient.ClusterV1alpha1().MachineSets(remoteClusterAPINamespace).Create(ms)
		if err != nil {
			return err
		}
	}

	for _, ms := range machineSetsToUpdate {
		c.logger.Debugf("updating ms %v", ms)
		_, err = remoteClusterAPIClient.ClusterV1alpha1().MachineSets(remoteClusterAPINamespace).Update(ms)
		if err != nil {
			return err
		}
	}

	for _, ms := range machineSetsToDelete {
		c.logger.Debugf("deleting ms %v", ms)
		err = remoteClusterAPIClient.ClusterV1alpha1().MachineSets(remoteClusterAPINamespace).Delete(ms.Name, &metav1.DeleteOptions{})
		if err != nil {
			return err
		}
	}

	return nil
}

// return list of non-master machinesets from a given ClusterDeployment
func computeClusterAPIMachineSetsFromClusterDeployment(clusterDeployment *cov1.ClusterDeployment, coClusterVersion *cov1.ClusterVersion) ([]*clusterapiv1.MachineSet, error) {
	machineSets := []*clusterapiv1.MachineSet{}

	for _, ms := range clusterDeployment.Spec.MachineSets {
		if ms.NodeType == cov1.NodeTypeMaster {
			continue
		}
		newMS, err := controller.BuildClusterAPIMachineSet(&ms, &clusterDeployment.Spec, coClusterVersion, remoteClusterAPINamespace)
		if err != nil {
			return nil, err
		}
		machineSets = append(machineSets, newMS)
	}
	return machineSets, nil
}
