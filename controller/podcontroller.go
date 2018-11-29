package controller

/*
Copyright 2017-2018 Crunchy Data Solutions, Inc.
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

import (
	"context"
	log "github.com/Sirupsen/logrus"
	"github.com/crunchydata/postgres-operator/kubeapi"
	clusteroperator "github.com/crunchydata/postgres-operator/operator/cluster"
	taskoperator "github.com/crunchydata/postgres-operator/operator/task"
	"github.com/crunchydata/postgres-operator/util"
	apiv1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
)

// PodController holds the connections for the controller
type PodController struct {
	PodClient    *rest.RESTClient
	PodClientset *kubernetes.Clientset
	Namespace    string
}

// Run starts an pod resource controller
func (c *PodController) Run(ctx context.Context) error {

	_, err := c.watchPods(ctx)
	if err != nil {
		log.Errorf("Failed to register watch for pod resource: %v", err)
		return err
	}

	<-ctx.Done()
	return ctx.Err()
}

// watchPods is the event loop for pod resources
func (c *PodController) watchPods(ctx context.Context) (cache.Controller, error) {
	source := cache.NewListWatchFromClient(
		c.PodClientset.CoreV1().RESTClient(),
		"pods",
		c.Namespace,
		fields.Everything())

	_, controller := cache.NewInformer(
		source,

		// The object type.
		&apiv1.Pod{},

		// resyncPeriod
		// Every resyncPeriod, all resources in the cache will retrigger events.
		// Set to 0 to disable the resync.
		0,

		// Your custom resource event handlers.
		cache.ResourceEventHandlerFuncs{
			AddFunc:    c.onAdd,
			UpdateFunc: c.onUpdate,
			DeleteFunc: c.onDelete,
		})

	go controller.Run(ctx.Done())
	return controller, nil
}

// onAdd is called when a pgcluster is added
func (c *PodController) onAdd(obj interface{}) {
	newpod := obj.(*apiv1.Pod)
	log.Debugf("[PodCONTROLLER] OnAdd %s", newpod.ObjectMeta.SelfLink)
	c.checkPostgresPods(newpod)
}

// onUpdate is called when a pgcluster is updated
func (c *PodController) onUpdate(oldObj, newObj interface{}) {
	oldpod := oldObj.(*apiv1.Pod)
	newpod := newObj.(*apiv1.Pod)
	log.Debugf("[PodCONTROLLER] OnUpdate %s", newpod.ObjectMeta.SelfLink)
	c.checkReadyStatus(oldpod, newpod)
}

// onDelete is called when a pgcluster is deleted
func (c *PodController) onDelete(obj interface{}) {
	pod := obj.(*apiv1.Pod)
	log.Debugf("[PodCONTROLLER] OnDelete %s", pod.ObjectMeta.SelfLink)
}

func (c *PodController) checkReadyStatus(oldpod, newpod *apiv1.Pod) {
	//if the pod has a metadata label of  pg-cluster and
	//eventually pg-failover == true then...
	//loop thru status.containerStatuses, find the container with name='database'
	//print out the 'ready' bool
	if newpod.ObjectMeta.Labels[util.LABEL_PRIMARY] == "true" &&
		newpod.ObjectMeta.Labels[util.LABEL_PG_CLUSTER] != "" &&
		newpod.ObjectMeta.Labels[util.LABEL_AUTOFAIL] == "true" {
		log.Infof("an autofail pg-cluster %s!", newpod.ObjectMeta.Labels[util.LABEL_PG_CLUSTER])
		for _, v := range newpod.Status.ContainerStatuses {
			if v.Name == "database" {
				clusteroperator.AutofailBase(c.PodClientset, c.PodClient, v.Ready, newpod.ObjectMeta.Labels[util.LABEL_PG_CLUSTER], newpod.ObjectMeta.Namespace)
			}
		}
	}

	//handle applying policies after a database is made Ready
	if newpod.ObjectMeta.Labels[util.LABEL_PRIMARY] == "true" {
		for _, v := range newpod.Status.ContainerStatuses {
			if v.Name == "database" {
				//see if there are pgtasks for adding a policy
				if v.Ready {
					log.Debugf("%s went to Ready, apply policies...", newpod.ObjectMeta.Labels[util.LABEL_PG_CLUSTER])
					taskoperator.ApplyPolicies(newpod.ObjectMeta.Labels[util.LABEL_PG_CLUSTER], c.PodClientset, c.PodClient)
					taskoperator.CompleteCreateClusterWorkflow(newpod.ObjectMeta.Labels[util.LABEL_PG_CLUSTER], c.PodClientset, c.PodClient)
				}
			}
		}
	}

}

/**
func checkReadyStatus(oldpod, newpod *apiv1.Pod) {
	//if the pod has a metadata label of  pg-cluster and
	//eventually pg-failover == true then...
	//loop thru status.containerStatuses, find the container with name='database'
	//print out the 'ready' bool
	log.Infof("%v is the ObjectMeta  Labels\n", newpod.ObjectMeta.Labels)
	if newpod.ObjectMeta.Labels["pg-cluster"] != "" {
		log.Infoln("we have a pg-cluster!")
		for _, v := range newpod.Status.ContainerStatuses {
			if v.Name == "database" {
				log.Infof("%s is the containerstatus Name\n", v.Name)
				if v.Ready {
					log.Infof("%v is the Ready status for cluster %s container %s container\n", v.Ready, newpod.ObjectMeta.Name, v.Name)
				} else {
					log.Infof("%v is the Ready status for cluster %s container %s container\n", v.Ready, newpod.ObjectMeta.Name, v.Name)
				}
			}
		}
	}

}

*/

// checkPostgresPods
// see if this is a primary or replica being created
// update service-name label on the pod for each case
// to match the correct Service selector for the PG cluster
func (c *PodController) checkPostgresPods(newpod *apiv1.Pod) {

	//you have a race condition with Kube events here we
	//are dealing with, the pod version can change between getting
	//the pod and updating it here, so we will retry a few times
	tries := 8
	for i := 0; i < tries; i++ {
		if newpod.ObjectMeta.Labels[util.LABEL_PRIMARY] == "true" || newpod.ObjectMeta.Labels[util.LABEL_PRIMARY] == "false" {
			pod, _, err := kubeapi.GetPod(c.PodClientset, newpod.Name, c.Namespace)
			if err != nil {
				log.Error(err)
				log.Error(" could not get pod %s", newpod.Name)
				return
			}

			if newpod.ObjectMeta.Labels[util.LABEL_PRIMARY] == "true" {
				log.Debugf("primary pod ADDED %s service-name=%s", newpod.Name, newpod.ObjectMeta.Labels[util.LABEL_PG_CLUSTER])
				//add label onto pod "service-name=clustername"
				pod.ObjectMeta.Labels[util.LABEL_SERVICE_NAME] = newpod.ObjectMeta.Labels[util.LABEL_PG_CLUSTER]
			} else if newpod.ObjectMeta.Labels[util.LABEL_PRIMARY] == "false" {
				log.Debugf("replica pod ADDED %s service-name=%s", newpod.Name, newpod.ObjectMeta.Labels[util.LABEL_PG_CLUSTER]+"-replica")
				//add label onto pod "service-name=clustername-replica"
				pod.ObjectMeta.Labels[util.LABEL_SERVICE_NAME] = newpod.ObjectMeta.Labels[util.LABEL_PG_CLUSTER] + "-replica"
			}

			err = kubeapi.UpdatePod(c.PodClientset, pod, c.Namespace)
			if err == nil {
				break
			} else {
				log.Error(err)
				log.Errorf(" could not update pod label for pod %s and label %s will try update again...", pod.Name, pod.ObjectMeta.Labels[util.LABEL_SERVICE_NAME])
			}
		}
	}

}
