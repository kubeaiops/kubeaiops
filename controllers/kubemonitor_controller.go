/*
Copyright 2023.

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

package controllers

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/config"
	"sigs.k8s.io/controller-runtime/pkg/log"

	aiopsv1alpha1 "github.com/kubeaiops/kubeaiops/api/v1alpha1"
	"github.com/robfig/cron/v3"

	//added codes
	argowfv1alpha1 "github.com/argoproj/argo-workflows/v3/pkg/apis/workflow/v1alpha1"
	argoclientset "github.com/argoproj/argo-workflows/v3/pkg/client/clientset/versioned"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/yaml"
)

// KubeMonitorReconciler reconciles a KubeMonitor object
type KubeMonitorReconciler struct {
	client.Client
	Scheme     *runtime.Scheme
	cron       *cron.Cron
	kubeclient *kubernetes.Clientset
}

//+kubebuilder:rbac:groups=aiops.kubeaiops.com,resources=kubemonitors,verbs=get;list;watch;create;update;patch;delete]
//+kubebuilder:rbac:groups=aiops.kubeaiops.com,resources=kubemonitors/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=aiops.kubeaiops.com,resources=kubemonitors/finalizers,verbs=update

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// TODO(user): Modify the Reconcile function to compare the state specified by
// the KubeMonitor object against the actual cluster state, and then
// perform operations to make the cluster state reflect the state specified by
// the user.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.14.1/pkg/reconcile

func (r *KubeMonitorReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {

	log := log.FromContext(ctx)

	if r.cron == nil {
		r.cron = cron.New()
	} else {
		r.cron.Stop()
	}

	// Fetch the custom resource
	var kubeMonitor = &aiopsv1alpha1.KubeMonitor{}
	if err := r.Get(ctx, req.NamespacedName, kubeMonitor); err != nil {
		if errors.IsNotFound(err) {
			// The custom resource was deleted, nothing to do
			log.Info("no resource")
			return ctrl.Result{}, nil
		}
		log.Error(err, "unable to fetch KubeMonitor")
		return ctrl.Result{}, err
	}

	defer func() {
		if r := recover(); r != nil {
			log.Info("Review the logs: %v", r)
		}
	}()

	_, err := r.cron.AddFunc(kubeMonitor.Spec.Cron, func() {
		argowf, err := r.argoWorkflowForKubeMonitor(kubeMonitor)
		if err != nil {
			log.Error(err, "failed to create argo workflow from Service")
		}
		log.Info("Creating a new Workflow", "Workflow.Namespace", argowf.Namespace, "Workflow.Name", argowf.Name)
		found := &argowfv1alpha1.Workflow{}
		if err := r.Get(ctx, types.NamespacedName{Name: argowf.Name, Namespace: argowf.Namespace}, found); err != nil && errors.IsNotFound(err) {
			if err := r.Create(ctx, argowf); err != nil {
				log.Error(err, "Failed to create new Workflow", "Workflow.Namespace", argowf.Namespace, "Workflow.Name", argowf.Name)
				//return ctrl.Result{}, err
			}
			generatedName := argowf.GetName()
			log.Info("Created Workflow", "generatedName", generatedName)
			// Workflow created successfully - return and requeue
		} else if err != nil {
			log.Info("Failed to get argo Workflow")
		}
		log.Info("Found argo Workflow", "name", found.Name, "namespace", found.Namespace)

		// Delete all but the most recent 5 workflows.
		if err := r.deleteOldWorkflows(ctx, kubeMonitor.Spec.Workflow.Selector, kubeMonitor.Spec.Workflow.Namespace, kubeMonitor.Spec.MaxWorkflowCount); err != nil {
			log.Error(err, "failed to delete old workflows")
		}
	})

	if err != nil {
		log.Error(err, "Failed to on Workflow", kubeMonitor.Name)
		return ctrl.Result{}, err
	}
	r.cron.Start()
	return ctrl.Result{}, nil
}

func (r *KubeMonitorReconciler) argoWorkflowForKubeMonitor(km *aiopsv1alpha1.KubeMonitor) (*argowfv1alpha1.Workflow, error) {

	wf := &argowfv1alpha1.Workflow{}
	b, err := yaml.ToJSON([]byte(km.Spec.Workflow.Source))
	if err != nil {
		return nil, err
	}

	if err := json.Unmarshal(b, wf); err != nil {
		return nil, err
	}

	wf.Namespace = km.Spec.Workflow.Namespace
	//wf.Name = km.Spec.Workflow.Name

	//wf.Spec.Arguments.Parameters = append(wf.Spec.Arguments.Parameters, []v1alpha1.Parameters{}... )
	// Set argo Workflow instance as the owner and controller
	ctrl.SetControllerReference(km, wf, r.Scheme)
	if err != nil {
		return nil, err
	}

	return wf, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *KubeMonitorReconciler) SetupWithManager(mgr ctrl.Manager) error {
	r.kubeclient = kubernetes.NewForConfigOrDie(mgr.GetConfig())
	return ctrl.NewControllerManagedBy(mgr).
		For(&aiopsv1alpha1.KubeMonitor{}).
		Complete(r)
}

func (r *KubeMonitorReconciler) deleteOldWorkflows(ctx context.Context, selector string, namespace string, numToKeep int) error {
	config, err := config.GetConfig()
	if err != nil {
		return err
	}
	// List all workflows in the namespace
	labelSelector := fmt.Sprintf("aiops.kubeaiops.com/selector=%s,workflows.argoproj.io/completed=true", selector)
	clientset := argoclientset.NewForConfigOrDie(config).ArgoprojV1alpha1().Workflows(namespace)
	workflows, err := clientset.List(ctx, metav1.ListOptions{
		LabelSelector: labelSelector,
	})
	if err != nil {
		return err
	}

	// Sort workflows by creation timestamp (newest to oldest)
	sort.Slice(workflows.Items, func(i, j int) bool {
		return workflows.Items[i].CreationTimestamp.Before(&workflows.Items[j].CreationTimestamp)
	})
	// Delete all but the most recent 5 workflows
	var numDeleted int
	numWorkflows := len(workflows.Items)
	fmt.Println("number of workflows:", numWorkflows, "selector: ", selector)
	fmt.Println("max limit of workflows:", numToKeep, "selector: ", selector)

	if numWorkflows >= numToKeep {
		for i := 0; i <= numWorkflows-numToKeep; i++ {
			workflowName := workflows.Items[i].Name
			err := clientset.Delete(ctx, workflowName, metav1.DeleteOptions{})
			if err != nil {
				return err
			}
			numDeleted++
		}
	}
	fmt.Printf("Deleted %d old workflows in namespace %s\n", numDeleted, namespace)
	return nil
}

// to import argoproject
var _ = schema.GroupVersionResource{
	Group:    "argoproj.io",
	Version:  "v1alpha1",
	Resource: "workflows",
}
