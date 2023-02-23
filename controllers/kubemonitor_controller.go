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

	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	aiopsv1alpha1 "github.com/kubeaiops/kubeaiops/api/v1alpha1"
	"github.com/robfig/cron/v3"

	//added codes

	argowfv1alpha1 "github.com/argoproj/argo-workflows/v3/pkg/apis/workflow/v1alpha1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/yaml"
)

// KubeMonitorReconciler reconciles a KubeMonitor object
type KubeMonitorReconciler struct {
	client.Client
	Scheme *runtime.Scheme
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
	c := cron.New()
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

	_, err := c.AddFunc(kubeMonitor.Spec.Cron, func() {
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
	})

	if err != nil {
		log.Error(err, "Failed to on Workflow", kubeMonitor.Name)
		return ctrl.Result{}, err
	}
	c.Start()
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
	wf.Name = km.Spec.Workflow.Name

	// Set argo Workflow instance as the owner and controller
	ctrl.SetControllerReference(km, wf, r.Scheme)
	if err != nil {
		return nil, err
	}

	return wf, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *KubeMonitorReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&aiopsv1alpha1.KubeMonitor{}).
		Complete(r)
}

func containsString(slice []string, item string) bool {
	for _, s := range slice {
		if s == item {
			return true
		}
	}
	return false
}

func removeString(slice []string, item string) []string {
	for i, s := range slice {
		if s == item {
			return append(slice[:i], slice[i+1:]...)
		}
	}
	return slice
}

// to import argoproject
var _ = schema.GroupVersionResource{
	Group:    "argoproj.io",
	Version:  "v1alpha1",
	Resource: "workflows",
}
