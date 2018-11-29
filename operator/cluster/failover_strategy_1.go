// Package cluster holds the cluster CRD logic and definitions
// A cluster is comprised of a primary service, replica service,
// primary deployment, and replica deployment
package cluster

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
	"encoding/json"
	"errors"
	log "github.com/Sirupsen/logrus"
	crv1 "github.com/crunchydata/postgres-operator/apis/cr/v1"
	"github.com/crunchydata/postgres-operator/kubeapi"
	"github.com/crunchydata/postgres-operator/util"
	jsonpatch "github.com/evanphx/json-patch"
	"k8s.io/api/core/v1"
	"k8s.io/api/extensions/v1beta1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

// AddCluster ...
func (r Strategy1) Failover(clientset *kubernetes.Clientset, client *rest.RESTClient, clusterName string, task *crv1.Pgtask, namespace string, restconfig *rest.Config) error {

	var pod *v1.Pod
	var err error
	target := task.ObjectMeta.Labels[util.LABEL_TARGET]

	log.Info("strategy 1 Failover called on " + clusterName + " target is " + target)

	pod, err = util.GetPod(clientset, target, namespace)
	if err != nil {
		log.Error(err)
		return err
	}
	log.Debugf("best pod to failover to is %s", pod.Name)

	//delete the primary deployment
	err = deletePrimary(clientset, namespace, clusterName)
	if err != nil {
		log.Error(err)
		return err
	}
	updateFailoverStatus(client, task, namespace, clusterName, "deleting primary deployment "+clusterName)

	//trigger the failover on the replica
	err = promote(pod, clientset, client, namespace, restconfig)
	updateFailoverStatus(client, task, namespace, clusterName, "promoting pod "+pod.Name+" target "+target)

	//drain the deployment, this will shutdown the database pod
	//err = kubeapi.PatchReplicas(clientset, target, namespace, "/spec/replicas", 0)
	//	if err != nil {
	//		log.Error(err)
	//		return err
	//	}

	//relabel the deployment with primary labels
	//by setting service-name=clustername
	var upod *v1.Pod
	upod, _, err = kubeapi.GetPod(clientset, pod.Name, namespace)
	if err != nil {
		log.Error(err)
		log.Error("error in getting pod during failover relabel")
		return err
	}

	//set the service-name label to the cluster name to match
	//the primary service selector
	//upod.ObjectMeta.Labels[util.LABEL_SERVICE_NAME] = pod.ObjectMeta.Labels[util.LABEL_PG_CLUSTER]
	upod.ObjectMeta.Labels[util.LABEL_SERVICE_NAME] = clusterName

	err = kubeapi.UpdatePod(clientset, upod, namespace)
	if err != nil {
		log.Error(err)
		log.Error("error in updating pod during failover relabel")
		return err
	}

	//err = relabel(pod, clientset, namespace, clusterName, target)

	updateFailoverStatus(client, task, namespace, clusterName, "re-labeling deployment...pod "+pod.Name+"was the failover target...failover completed")

	//enable the deployment by making replicas equal to 1
	//	err = kubeapi.PatchReplicas(clientset, target, namespace, "/spec/replicas", 1)
	//	if err != nil {
	//		log.Error(err)
	//		return err
	//	}

	return err

}

func updateFailoverStatus(client *rest.RESTClient, task *crv1.Pgtask, namespace, clusterName, message string) {

	log.Debugf("updateFailoverStatus namespace=[%s] taskName=[%s] message=[%s]", namespace, task.Name, message)

	//update the task

	_, err := kubeapi.Getpgtask(client, task, task.ObjectMeta.Name,
		task.ObjectMeta.Namespace)
	if err != nil {
		return
	}

	task.Status.Message = message

	err = kubeapi.Updatepgtask(client,
		task,
		task.ObjectMeta.Name,
		task.ObjectMeta.Namespace)
	if err != nil {
		return
	}

}

func deletePrimary(clientset *kubernetes.Clientset, namespace, clusterName string) error {

	//the primary will be the one with a pod that has a label
	//that looks like service-name=clustername
	selector := util.LABEL_SERVICE_NAME + "=" + clusterName
	pods, err := kubeapi.GetPods(clientset, selector, namespace)
	if len(pods.Items) == 0 {
		log.Errorf("no primary pod found when trying to delete primary %s", selector)
		return errors.New("could not find primary pod")
	}
	if len(pods.Items) > 1 {
		log.Errorf("more than 1 primary pod found when trying to delete primary %s", selector)
		return errors.New("more than 1 primary pod found in delete primary logic")
	}

	deploymentToDelete := pods.Items[0].ObjectMeta.Labels[util.LABEL_DEPLOYMENT_NAME]

	//delete the deployment with pg-cluster=clusterName,primary=true
	//should only be 1 primary with this name!
	//deps, err := kubeapi.GetDeployments(clientset, util.LABEL_PG_CLUSTER+"="+clusterName+",primary=true", namespace)
	//for _, d := range deps.Items {
	//	log.Debugf("deleting deployment %s", d.Name)
	//	kubeapi.DeleteDeployment(clientset, d.Name, namespace)
	//}
	log.Debugf("deleting deployment %s", deploymentToDelete)
	err = kubeapi.DeleteDeployment(clientset, deploymentToDelete, namespace)

	return err
}

func promote(
	pod *v1.Pod,
	clientset *kubernetes.Clientset,
	client *rest.RESTClient, namespace string, restconfig *rest.Config) error {

	//get the target pod that matches the replica-name=target

	command := make([]string, 1)
	command[0] = "/opt/cpm/bin/promote.sh"

	log.Debugf("running Exec with namespace=[%s] podname=[%s] container name=[%s]", namespace, pod.Name, pod.Spec.Containers[0].Name)
	stdout, stderr, err := kubeapi.ExecToPodThroughAPI(restconfig, clientset, command, pod.Spec.Containers[0].Name, pod.Name, namespace, nil)
	log.Debugf("stdout=[%s] stderr=[%s]", stdout, stderr)
	if err != nil {
		log.Error(err)
	}

	return err
}

func relabel(pod *v1.Pod, clientset *kubernetes.Clientset, namespace, clusterName, target string) error {
	var err error

	targetDeployment, found, err := kubeapi.GetDeployment(clientset, target, namespace)
	if !found {
		return err
	}

	//set primary=true on the deployment
	//set name=clustername on the deployment
	newLabels := make(map[string]string)
	newLabels[util.LABEL_NAME] = clusterName
	newLabels[util.LABEL_PRIMARY] = "true"

	err = updateLabels(namespace, clientset, targetDeployment, target, newLabels)
	if err != nil {
		log.Error(err)
	}

	err = kubeapi.MergePatchDeployment(clientset, targetDeployment, clusterName, namespace)
	if err != nil {
		log.Error(err)
	}

	return err
}

// TODO this code came mostly from util/util.go...refactor to merge
func updateLabels(namespace string, clientset *kubernetes.Clientset, deployment *v1beta1.Deployment, clusterName string, newLabels map[string]string) error {

	var err error

	log.Debugf("%v is the labels to apply", newLabels)

	var patchBytes, newData, origData []byte
	origData, err = json.Marshal(deployment)
	if err != nil {
		return err
	}

	accessor, err2 := meta.Accessor(deployment)
	if err2 != nil {
		return err2
	}

	objLabels := accessor.GetLabels()
	if objLabels == nil {
		objLabels = make(map[string]string)
	}
	log.Debugf("current labels are %v", objLabels)

	//update the deployment labels
	for key, value := range newLabels {
		objLabels[key] = value
	}
	log.Debugf("updated labels are %v", objLabels)

	accessor.SetLabels(objLabels)

	newData, err = json.Marshal(deployment)
	if err != nil {
		return err
	}
	patchBytes, err = jsonpatch.CreateMergePatch(origData, newData)
	if err != nil {
		return err
	}

	_, err = clientset.ExtensionsV1beta1().Deployments(namespace).Patch(clusterName, types.MergePatchType, patchBytes, "")
	if err != nil {
		log.Debugf("error patching deployment %s", err.Error())
	}
	return err

}

func validateDBContainer(pod *v1.Pod) bool {
	found := false

	for _, c := range pod.Spec.Containers {
		if c.Name == "database" {
			return true
		}
	}
	return found

}
