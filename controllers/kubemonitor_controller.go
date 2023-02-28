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
	"reflect"
	"sort"
	"time"

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
	Scheme               *runtime.Scheme
	cron                 *cron.Cron
	kubeclient           *kubernetes.Clientset
	lastSpec             aiopsv1alpha1.KubeMonitorSpec
	generatedName        string
	workflowStatus       argowfv1alpha1.WorkflowPhase
	lastUpdateTimeMetav1 metav1.Time
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
	defer func() {
		if r := recover(); r != nil {
			log.Info("Review the logs: %v", r)
		}
	}()

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

	// Check if the spec of the custom resource has changed since the last reconcile
	if reflect.DeepEqual(kubeMonitor.Spec, r.lastSpec) {
		return ctrl.Result{}, nil
	}

	// Store the current spec for the next reconcile loop
	r.lastSpec = kubeMonitor.Spec

	err := r.scheduleWorkflow(ctx, kubeMonitor)
	if err != nil {
		log.Error(err, "Failed to on Workflow", kubeMonitor.Name)
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

func (r *KubeMonitorReconciler) scheduleWorkflow(ctx context.Context, km *aiopsv1alpha1.KubeMonitor) error {

	log := log.FromContext(ctx)

	if r.cron == nil {
		r.cron = cron.New()
		r.cron.Start()
	}

	//var generatedName string
	//var workflowStatus argowfv1alpha1.WorkflowPhase
	//var lastUpdateTimeMetav1 metav1.Time
	errCh := make(chan error)

	go func() {
		_, err := r.cron.AddFunc(km.Spec.Cron, func() {
			argowf, err := r.argoWorkflowForKubeMonitor(km)
			if err != nil {
				errCh <- err
				return
			}

			log.Info("Creating a new Workflow", "Workflow.Namespace", argowf.Namespace)

			found := &argowfv1alpha1.Workflow{}
			if err := r.Get(ctx, types.NamespacedName{Name: argowf.Name, Namespace: argowf.Namespace}, found); err != nil && errors.IsNotFound(err) {
				if err := r.Create(ctx, argowf); err != nil {
					fmt.Println("can't fund workflow")
					errCh <- err
					return
				}

				r.generatedName = argowf.GetName()

				for {
					if err := r.Get(ctx, types.NamespacedName{Name: r.generatedName, Namespace: argowf.Namespace}, found); err == nil {
						if found.Status.Phase != "" {
							r.workflowStatus = found.Status.Phase
							break
						}
					}
					time.Sleep(1 * time.Second)
				}

				lastUpdateTime, err := time.Parse(time.RFC3339, found.ObjectMeta.CreationTimestamp.Format(time.RFC3339))
				if err != nil {
					errCh <- err
					return
				}
				r.lastUpdateTimeMetav1 = metav1.NewTime(lastUpdateTime)

				fmt.Println("-----Generated name---in go func", r.generatedName)
				fmt.Println("-----workflowStatus---in go func", string(r.workflowStatus))
				fmt.Println("-----lastUpdateTimeMetav1---in go func", r.lastUpdateTimeMetav1)

				if err := r.updateKubeMonitorStatus(ctx, km); err != nil {
					log.Error(err, "Failed to update KubeMonitor status", "KubeMonitor.Name", km.Name)
					errCh <- err
					return
				}

				if err := r.deleteOldWorkflows(ctx, km.Spec.Workflow.Selector, km.Spec.Workflow.Namespace, km.Spec.MaxWorkflowCount); err != nil {
					errCh <- err
					return
				}

			} else if err != nil {
				errCh <- err
				log.Info("what's happening?")
				return
			}

			log.Info("Found argo Workflow", "name", r.generatedName, "namespace", found.Namespace)
		})

		if err != nil {
			errCh <- err
			return
		}

		errCh <- nil
	}()

	if err := <-errCh; err != nil {
		return err
	}

	return nil
}

func (r *KubeMonitorReconciler) updateKubeMonitorStatus(ctx context.Context, km *aiopsv1alpha1.KubeMonitor) error {
	// Update the status of the custom resource
	log := log.FromContext(ctx)

	fmt.Println("generatedName-----------update", r.generatedName)
	fmt.Println("argowf.Status.Phase-----------update", string(r.workflowStatus))
	fmt.Println("lastUpdateTime-----------update", r.lastUpdateTimeMetav1)

	km.Status = aiopsv1alpha1.KubeMonitorStatus{
		WorkflowName:   r.generatedName,
		WorkflowStatus: string(r.workflowStatus),
		LastUpdateTime: r.lastUpdateTimeMetav1,
	}
	if err := r.Status().Update(ctx, km); err != nil {
		log.Error(err, "Failed to update KubeMonitor status", "KubeMonitor.Name", km.Name)
		return err
	}

	return nil
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
	fmt.Printf("Deleted %d old workflows in the namespace %s\n", numDeleted, namespace)
	return nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *KubeMonitorReconciler) SetupWithManager(mgr ctrl.Manager) error {
	r.kubeclient = kubernetes.NewForConfigOrDie(mgr.GetConfig())
	return ctrl.NewControllerManagedBy(mgr).
		For(&aiopsv1alpha1.KubeMonitor{}).
		Complete(r)
}

// to import argoproject
var _ = schema.GroupVersionResource{
	Group:    "argoproj.io",
	Version:  "v1alpha1",
	Resource: "workflows",
}
