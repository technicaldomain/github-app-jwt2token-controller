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

package main

import (
	"context"

	clientset "github.com/technicaldomain/github-app-jwt2token-controller/pkg/generated/clientset/versioned"
	"github.com/technicaldomain/github-app-jwt2token-controller/pkg/ghsutil"
)

const controllerAgentName = "github-app-jwt2token"

const (
	SuccessSynced       = "Synced"
	SuccessUpdated      = "Updated"
	MessageTokenValid   = "The token is still valid"
	MessageTokenUpdated = "The token has been refreshed"
	FieldManager        = controllerAgentName
)

func shouldRegenerateToken(ctx context.Context, clientset clientset.Interface, tockenHash string, namespace string) bool {
	ghs, err := ghsutil.GetGHS(ctx, clientset, tockenHash, namespace)
	if err != nil || ghs == nil {
		return true
	}
	return false
}
