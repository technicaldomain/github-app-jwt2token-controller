/*
Copyright 2025 Alexander Kharkevich.

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

package v1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// +genclient
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
type ArgoCDRepo struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ArgoCDRepoSpec   `json:"spec,omitempty"`
	Status ArgoCDRepoStatus `json:"status,omitempty"`
}

type ArgoCDRepoSpec struct {
	PrivateKeySecret   string               `json:"privateKeySecret"`
	ArgoCDRepositories []ArgoCDRepositories `json:"argoCDRepositories"`
}

type ArgoCDRepositories struct {
	Repository string `json:"repository"`
	Namespace  string `json:"namespace"`
}

type ArgoCDRepoStatus struct {
	Token     string      `json:"token,omitempty"`
	ExpiresAt metav1.Time `json:"expiresAt,omitempty"` // New field to track JWT expiration time
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
type ArgoCDRepoList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ArgoCDRepo `json:"items"`
}

// +genclient
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

type DockerConfigJson struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   DockerConfigJsonSpec   `json:"spec,omitempty"`
	Status DockerConfigJsonStatus `json:"status,omitempty"`
}

type DockerConfigJsonSpec struct {
	PrivateKeySecret    string                `json:"privateKeySecret"`
	DockerConfigSecrets []DockerConfigSecrets `json:"dockerConfigSecrets"`
}

type DockerConfigSecrets struct {
	Secret    string `json:"secret"`
	Namespace string `json:"namespace"`
	Registry  string `json:"registry" default:"ghcr.io"`
	Username  string `json:"username" default:"x-access-token"`
}

type DockerConfigJsonStatus struct {
	Token     string      `json:"token,omitempty"`
	ExpiresAt metav1.Time `json:"expiresAt,omitempty"`
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
type DockerConfigJsonList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []DockerConfigJson `json:"items"`
}
