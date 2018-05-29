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

package syncclusterdeployment

import (
	"bytes"
	"fmt"
	"time"

	"github.com/golang/glog"
	log "github.com/sirupsen/logrus"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	jsonserializer "k8s.io/apimachinery/pkg/runtime/serializer/json"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	kubeclientset "k8s.io/client-go/kubernetes"
	v1core "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/workqueue"

	coapi "github.com/openshift/cluster-operator/pkg/api"
	cov1 "github.com/openshift/cluster-operator/pkg/apis/clusteroperator/v1alpha1"
	coclient "github.com/openshift/cluster-operator/pkg/client/clientset_generated/clientset"
	informers "github.com/openshift/cluster-operator/pkg/client/informers_generated/externalversions/clusteroperator/v1alpha1"
	lister "github.com/openshift/cluster-operator/pkg/client/listers_generated/clusteroperator/v1alpha1"
	"github.com/openshift/cluster-operator/pkg/controller"
	"github.com/openshift/cluster-operator/pkg/kubernetes/pkg/util/metrics"

	clusterapiv1 "sigs.k8s.io/cluster-api/pkg/apis/cluster/v1alpha1"
	clusterapiclient "sigs.k8s.io/cluster-api/pkg/client/clientset_generated/clientset"
)

const (
	// maxRetries is the number of times a service will be retried before it is dropped out of the queue.
	// With the current rate-limiter in use (5ms*2^(maxRetries-1)) the following numbers represent the
	// sequence of delays between successive queuings of a machineset.
	//
	// 5ms, 10ms, 20ms, 40ms, 80ms, 160ms, 320ms, 640ms, 1.3s, 2.6s, 5.1s, 10.2s, 20.4s, 41s, 82s
	maxRetries = 15

	controllerLogName = "syncclusterdeployment"

	remoteClusterAPINamespace      = "kube-cluster"
	remoteClusterAPIDeploymentName = "cluster-api-apiserver"
	errorMsgClusterAPINotInstalled = "cannot sync until cluster API is installed and ready"
)

// NewController returns a new *Controller.
func NewController(
	clusterInformer informers.ClusterDeploymentInformer,
	kubeClient kubeclientset.Interface,
	clusteroperatorClient coclient.Interface,
) *Controller {
	eventBroadcaster := record.NewBroadcaster()
	eventBroadcaster.StartLogging(glog.Infof)
	// TODO: remove the wrapper when every clients have moved to use the clientset.
	eventBroadcaster.StartRecordingToSink(&v1core.EventSinkImpl{Interface: v1core.New(kubeClient.CoreV1().RESTClient()).Events("")})

	if kubeClient != nil && kubeClient.CoreV1().RESTClient().GetRateLimiter() != nil {
		metrics.RegisterMetricAndTrackRateLimiterUsage("clusteroperator_syncclusterdeployment_controller", kubeClient.CoreV1().RESTClient().GetRateLimiter())
	}

	logger := log.WithField("controller", controllerLogName)

	c := &Controller{
		client:     clusteroperatorClient,
		kubeClient: kubeClient,
		queue:      workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), "syncclusterdeployment"),
		logger:     logger,
	}

	clusterInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    c.addCluster,
		UpdateFunc: c.updateCluster,
	})
	c.clusterLister = clusterInformer.Lister()
	c.clustersSynced = clusterInformer.Informer().HasSynced

	c.syncHandler = c.syncClusterDeployment
	c.enqueueCluster = c.enqueue
	c.BuildRemoteClient = c.buildRemoteClusterClients

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
	enqueueCluster func(cluster *cov1.ClusterDeployment)

	BuildRemoteClient func(*cov1.ClusterDeployment) (clusterapiclient.Interface, error)

	// clusterLister is able to list/get clusters and is populated
	// by the shared informer passed to NewController.
	clusterLister lister.ClusterDeploymentLister
	// clustersSynced returns true if the cluster shared informer
	// has been synced at least once. Added as a member to the
	// struct to allow injection for testing.
	clustersSynced cache.InformerSynced

	// Machines that need to be synced
	queue workqueue.RateLimitingInterface

	logger log.FieldLogger
}

func (c *Controller) addCluster(obj interface{}) {
	cluster := obj.(*cov1.ClusterDeployment)
	c.logger.Debugf("syncclusterdeployment: adding cluster %v", cluster.Name)
	c.enqueueCluster(cluster)
}

func (c *Controller) updateCluster(old, cur interface{}) {
	cluster := cur.(*cov1.ClusterDeployment)
	c.logger.Debugf("syncclusterdeployment: updating cluster %v", cluster.Name)
	c.enqueueCluster(cluster)
}

// Run runs c; will not return until stopCh is closed. workers determines how
// many machine sets will be handled in parallel.
func (c *Controller) Run(workers int, stopCh <-chan struct{}) {
	defer utilruntime.HandleCrash()
	defer c.queue.ShutDown()

	c.logger.Infof("Starting syncclusterdeployment controller")
	defer c.logger.Infof("Shutting down syncclusterdeployment controller")

	if !controller.WaitForCacheSync("syncclusterdeployment", stopCh, c.clustersSynced) {
		c.logger.Errorf("could not sync caches for syncclusterdeployment controller")
		return
	}

	for i := 0; i < workers; i++ {
		go wait.Until(c.worker, time.Second, stopCh)
	}

	<-stopCh
}

func (c *Controller) enqueue(cluster *cov1.ClusterDeployment) {
	key, err := controller.KeyFunc(cluster)
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("couldn't get key for object %#v: %v", cluster, err))
		return
	}
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

	logger := c.logger.WithField("machineset", key)

	if c.queue.NumRequeues(key) < maxRetries {
		logger.Infof("error syncing machine set: %v", err)
		c.queue.AddRateLimited(key)
		return
	}

	utilruntime.HandleError(err)
	logger.Infof("dropping machine set out of the queue: %v", err)
	c.queue.Forget(key)
}

func isMasterMachineSet(machineSet *cov1.ClusterMachineSet) bool {
	return machineSet.NodeType == cov1.NodeTypeMaster
}

func (c *Controller) buildRemoteClusterClients(cluster *cov1.ClusterDeployment) (clusterapiclient.Interface, error) {
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
	secretName := cluster.Name + "-kubeconfig"

	// Step 2: Retrieve secret
	secret, err := c.kubeClient.CoreV1().Secrets(cluster.Namespace).Get(secretName, metav1.GetOptions{})
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
	c.logger.WithField("key", key).Debug("syncing machineset")
	defer func() {
		c.logger.WithFields(log.Fields{
			"key":      key,
			"duration": time.Now().Sub(startTime),
		}).Debug("finished syncing machineset")
	}()

	namespace, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		return err
	}

	cluster, err := c.clusterLister.ClusterDeployments(namespace).Get(name)
	if err != nil {
		c.logger.Errorf("Error fetching cluster %v/%v", namespace, name)
		return err
	}

	err = c.syncMachineSets(cluster)

	return err
}

// makes sure the remote cluster's MachineSets match the provided ClusterDeployment.Spec.MachineSets[]
func (c *Controller) syncMachineSets(cluster *cov1.ClusterDeployment) error {
	if !cluster.Status.ClusterAPIInstalled {
		c.logger.Debugf(errorMsgClusterAPINotInstalled)
		return nil
	}

	machineSetsToDelete := []*clusterapiv1.MachineSet{}
	machineSetsToCreate := []*clusterapiv1.MachineSet{}
	machineSetsToUpdate := []*clusterapiv1.MachineSet{}

	remoteClusterAPIClient, err := c.BuildRemoteClient(cluster)
	if err != nil {
		return err
	}
	remoteMachineSets, err := remoteClusterAPIClient.ClusterV1alpha1().MachineSets(remoteClusterAPINamespace).List(metav1.ListOptions{})

	coVersionNamespace, coVersionName := cluster.Spec.ClusterVersionRef.Namespace, cluster.Spec.ClusterVersionRef.Name
	coClusterVersion, err := c.client.ClusteroperatorV1alpha1().ClusterVersions(coVersionNamespace).Get(coVersionName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("cannot retrieve cluster version %s/%s: %v", coVersionNamespace, coVersionName, err)
	}

	clusterDeploymentComputeMachineSets := computeClusterAPIMachineSetsFromClusterDeployment(cluster, coClusterVersion)

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

	c.logger.Debugf("Creating machineset(s) %+v", machineSetsToCreate)
	c.logger.Debugf("Updating machineset(s) %+v", machineSetsToUpdate)
	c.logger.Debugf("Deleting machineset(s) %+v", machineSetsToDelete)

	for _, ms := range machineSetsToCreate {
		c.logger.Debugf("creating ms %v", ms)
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
func computeClusterAPIMachineSetsFromClusterDeployment(cluster *cov1.ClusterDeployment, coClusterVersion *cov1.ClusterVersion) []*clusterapiv1.MachineSet {
	machineSets := []*clusterapiv1.MachineSet{}

	for _, ms := range cluster.Spec.MachineSets {
		if ms.NodeType == cov1.NodeTypeMaster {
			continue
		}
		newMS, _ := buildClusterAPIMachineSet(&ms, coClusterVersion)
		machineSets = append(machineSets, newMS)
	}
	return machineSets
}

func buildClusterAPIMachineSet(ms *cov1.ClusterMachineSet, clusterVersion *cov1.ClusterVersion) (*clusterapiv1.MachineSet, error) {
	capiMachineSet := clusterapiv1.MachineSet{}
	capiMachineSet.Name = ms.ShortName
	capiMachineSet.Namespace = remoteClusterAPINamespace
	replicas := int32(ms.Size)
	capiMachineSet.Spec.Replicas = &replicas
	capiMachineSet.Spec.Selector.MatchLabels = map[string]string{"machineset": ms.ShortName}

	/*
		// TODO: figure out what to do about annotations
		if machineSet.Annotations == nil {
			machineSet.Annotations = map[string]string{}
		}
		sClusterVersion, err := serializeCOResource(clusterVersion)
		if err != nil {
			return nil, err
		}
		machineSet.Annotations["cluster-operator.openshift.io/cluster-version"] = string(sClusterVersion)
	*/

	machineTemplate := clusterapiv1.MachineTemplateSpec{}
	machineTemplate.Labels = map[string]string{"machineset": ms.ShortName}
	machineTemplate.Spec.Labels = map[string]string{"machineset": ms.ShortName}

	capiMachineSet.Spec.Template = machineTemplate

	return &capiMachineSet, nil
}

func buildClusterAPICluster(cluster *cov1.ClusterDeployment) (*clusterapiv1.Cluster, error) {
	capiCluster := clusterapiv1.Cluster{}
	capiCluster.Name = cluster.Name
	sCluster, err := serializeCOResource(cluster)
	if err != nil {
		return nil, err
	}
	capiCluster.Spec.ProviderConfig.Value = &runtime.RawExtension{
		Raw: sCluster,
	}

	// These are unused, dummy values.
	capiCluster.Spec.ClusterNetwork.ServiceDomain = "cluster-api.k8s.io"
	capiCluster.Spec.ClusterNetwork.Pods.CIDRBlocks = []string{"10.10.0.0/16"}
	capiCluster.Spec.ClusterNetwork.Services.CIDRBlocks = []string{"172.30.0.0/16"}

	return &capiCluster, nil
}

func serializeCOResource(object runtime.Object) ([]byte, error) {
	serializer := jsonserializer.NewSerializer(jsonserializer.DefaultMetaFactory, coapi.Scheme, coapi.Scheme, false)
	encoder := coapi.Codecs.EncoderForVersion(serializer, cov1.SchemeGroupVersion)
	buffer := &bytes.Buffer{}
	err := encoder.Encode(object, buffer)
	if err != nil {
		return nil, err
	}
	return buffer.Bytes(), nil
}
