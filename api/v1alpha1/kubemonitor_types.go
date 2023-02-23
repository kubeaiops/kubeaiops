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

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// EDIT THIS FILE!  THIS IS SCAFFOLDING FOR YOU TO OWN!
// NOTE: json tags are required.  Any new fields you add must have json tags for the fields to be serialized.

type Workflow struct {
	Name      string `json:"name,omitempty"`
	Namespace string `json:"namespace,omitempty"`
	Source    string `json:"source,omitempty"`
}

// KubeMonitorSpec defines the desired state of KubeMonitor
type KubeMonitorSpec struct {
	// INSERT ADDITIONAL SPEC FIELDS - desired state of cluster
	// Important: Run "make" to regenerate code after modifying this file
	Workflow         Workflow  `json:"workflow"`
	Cron             string    `json:"cron"`
	Arguments        Arguments `json:"arguments"`
	MaxWorkflowCount int       `json:"maxWorkflowCount"`
}

type Arguments struct {
	Parameters []Parameters `json:"parameters"`
}

type Parameters struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// KubeMonitorStatus defines the observed state of KubeMonitor
type KubeMonitorStatus struct {
	// INSERT ADDITIONAL STATUS FIELD - define observed state of cluster
	// Important: Run "make" to regenerate code after modifying this file
	CreatedAt    *metav1.Time `json:"createdAt,omitempty"`
	LastFailedAt *metav1.Time `json:"lastFailedAt,omitempty"`
	SuccessCount int          `json:"successCount,omitempty"`
	FailedCount  int          `json:"failedCount,omitempty"`
	TotalCount   int          `json:"totalCount,omitempty"`
	Status       string       `json:"status,omitempty"`
}

//+kubebuilder:object:root=true
//+kubebuilder:subresource:status

// KubeMonitor is the Schema for the kubemonitors API
type KubeMonitor struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   KubeMonitorSpec   `json:"spec,omitempty"`
	Status KubeMonitorStatus `json:"status,omitempty"`
}

//+kubebuilder:object:root=true

// KubeMonitorList contains a list of KubeMonitor
type KubeMonitorList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []KubeMonitor `json:"items"`
}

func init() {
	SchemeBuilder.Register(&KubeMonitor{}, &KubeMonitorList{})
}
