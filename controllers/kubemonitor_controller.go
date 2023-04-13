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
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/config"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/source"

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
	lastSpec   aiopsv1alpha1.KubeMonitorSpec
	lastStatus aiopsv1alpha1.KubeMonitorStatus
}

var (
	mu            sync.Mutex
	workflowsLock sync.Mutex
	workflows     = make(map[string]bool) // keeps track of workflows that have already been created
)

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
			// The custom resource was deleted...
			log.Info("KubeMonitor deleted")
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	// Check if the spec of the custom resource has changed since the last reconcile
	if reflect.DeepEqual(kubeMonitor.Spec, r.lastSpec) {
		return ctrl.Result{}, nil
	}

	// Store the current spec and status for the next reconcile loop
	r.lastSpec = kubeMonitor.Spec
	r.lastStatus = kubeMonitor.Status

	// Check if a workflow for this resource has already been created
	workflowsLock.Lock()
	if workflows[kubeMonitor.Name] {
		workflowsLock.Unlock()
		return ctrl.Result{}, nil
	}

	err := r.scheduleWorkflow(ctx, kubeMonitor)
	if err != nil {
		log.Error(err, "Failed to on Workflow", kubeMonitor.Name)
		workflowsLock.Unlock()
		return ctrl.Result{}, err
	}

	// Mark the workflow for this resource as created
	workflows[kubeMonitor.Name] = true
	workflowsLock.Unlock()

	return ctrl.Result{}, nil
}

type CustomCronJob struct {
	CronEntryID cron.EntryID
	jobFunc     func()
}

func (ccj *CustomCronJob) Run() {
	ccj.jobFunc()
}

func (r *KubeMonitorReconciler) scheduleWorkflow(ctx context.Context, km *aiopsv1alpha1.KubeMonitor) error {
	log := log.FromContext(ctx)
	errCh := make(chan error)

	go func() {
		customCronJob := &CustomCronJob{}
		customCronJob.jobFunc = func() {
			log.Info("Running cron job for KubeMonitor", "KubeMonitor.Name", km.Name, "KubeMonitor.Namespace", km.Namespace)
			// Check if the KubeMonitor resource still exists
			_, err := r.getKubeMonitor(ctx, km.Name, km.Namespace)
			if err != nil {
				// The KubeMonitor has been deleted, remove the cron job
				r.cron.Remove(customCronJob.CronEntryID)
				return
			}
			argowf, err := r.argoWorkflowForKubeMonitor(km)
			if err != nil {
				errCh <- err
				return
			}
			log.Info("Creating a new Workflow", "Workflow.Namespace", argowf.Namespace)
			found := &argowfv1alpha1.Workflow{}
			if err := r.Get(ctx, types.NamespacedName{Name: argowf.Name, Namespace: argowf.Namespace}, found); err != nil && errors.IsNotFound(err) {
				if err := r.Create(ctx, argowf); err != nil {
					errCh <- err
					return
				}
				// Spawn a goroutine to monitor the workflow status
				go monitorWorkflowStatus(ctx, r.Client, argowf.GetName(), argowf.Namespace, km)
				if err := r.deleteOldWorkflows(ctx, km.Spec.Workflow.Selector, km.Spec.Workflow.Namespace, km.Spec.MaxWorkflowCount); err != nil {
					errCh <- err
					return
				}
			} else if err != nil {
				errCh <- err
				return
			} else {
				log.Info("Workflow already exists", "Workflow.Namespace", argowf.Namespace)
			}
		}
		var err error
		customCronJob.CronEntryID, err = r.cron.AddJob(km.Spec.Cron, customCronJob)
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

func (r *KubeMonitorReconciler) deleteOldWorkflows(ctx context.Context, selector string, namespace string, numToKeep int) error {
	log := log.FromContext(ctx)
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
	log.Info(fmt.Sprintf("Deleted %d old workflows in the namespace %s", numDeleted, namespace))
	return nil
}

/*
func (r *KubeMonitorReconciler) updateKubeMonitorStatus(ctx context.Context, km *aiopsv1alpha1.KubeMonitor) error {
	// Update the status of the custom resource
	log := log.FromContext(ctx)

	//fmt.Println("generatedName-----------update", r.generatedName)
	//fmt.Println("argowf.Status.Phase-----------update", string(r.workflowStatus))
	//fmt.Println("lastUpdateTime-----------update", r.lastUpdateTimeMetav1)

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
*/

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
	const ownerKey = ".metadata.controller"

	for _, gvk := range []schema.GroupVersionKind{aiopsv1alpha1.SchemeBuilder.GroupVersion.WithKind("KubeMonitor")} {
		// Create a new controller for this GVK
		c, err := controller.New(fmt.Sprintf("%v-controller", gvk.Kind), mgr, controller.Options{
			Reconciler: r,
		})
		if err != nil {
			return err
		}
		// Watch the object type and enqueue object key
		if err := c.Watch(&source.Kind{Type: &aiopsv1alpha1.KubeMonitor{}}, &handler.EnqueueRequestForObject{}); err != nil {
			return err
		}
		// Set the owner reference of the child object to the parent object
		if err := mgr.GetFieldIndexer().IndexField(context.Background(), &aiopsv1alpha1.KubeMonitor{}, ownerKey, func(rawObj client.Object) []string {
			// grab the KubeMonitor object, extract the owner...
			kubeMonitor := rawObj.(*aiopsv1alpha1.KubeMonitor)
			owner := metav1.GetControllerOf(kubeMonitor)
			if owner == nil {
				return nil
			}
			// ...and if it's this object, add it to our list
			if owner.APIVersion == gvk.GroupVersion().String() && owner.Kind == gvk.Kind {
				return []string{owner.Name}
			}
			return nil
		}); err != nil {
			return err
		}
	}

	r.kubeclient = kubernetes.NewForConfigOrDie(mgr.GetConfig())
	r.cron = cron.New()
	r.cron.Start()

	return nil
}

// to import argoproject
var _ = schema.GroupVersionResource{
	Group:    "argoproj.io",
	Version:  "v1alpha1",
	Resource: "workflows",
}

func monitorWorkflowStatus(ctx context.Context, c client.Client, name string, namespace string, km *aiopsv1alpha1.KubeMonitor) {

	log := log.FromContext(ctx)

	for {
		wf := &argowfv1alpha1.Workflow{}
		i := 0
		for {
			if err := c.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, wf); err != nil {
				// wait 5 sec to get workflow, sometimes this function is called too fast to get a created workflow
				if i > 5 {
					log.Info("Can't find or deleted workflow: " + name)
					return
				}
			} else {
				break
			}
			i++
			time.Sleep(1 * time.Second)
		}

		if wf.Status.Phase == argowfv1alpha1.WorkflowSucceeded || wf.Status.Phase == argowfv1alpha1.WorkflowFailed || wf.Status.Phase == argowfv1alpha1.WorkflowRunning {
			mu.Lock()
			km.Status.WorkflowName = name
			km.Status.WorkflowStatus = string(wf.Status.Phase)
			lastUpdateTime, err := time.Parse(time.RFC3339, wf.ObjectMeta.CreationTimestamp.Format(time.RFC3339))
			if err != nil {
				log.Error(err, "error to convert CreationTimestamp - ")
				mu.Unlock()
				return
			}
			km.Status.LastUpdateTime = metav1.NewTime(lastUpdateTime)
			if err := c.Status().Update(ctx, km); err != nil {
				log.Error(err, "error to update KubeMonitor Status ")
				mu.Unlock()
				return
			}
			mu.Unlock()
			if wf.Status.Phase != argowfv1alpha1.WorkflowRunning {
				return
			}
		}
		time.Sleep(1 * time.Second)
	}
}

func (r *KubeMonitorReconciler) getKubeMonitor(ctx context.Context, name, namespace string) (*aiopsv1alpha1.KubeMonitor, error) {
	kubeMonitor := &aiopsv1alpha1.KubeMonitor{}
	if err := r.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, kubeMonitor); err != nil {
		return nil, err
	}
	return kubeMonitor, nil
}
