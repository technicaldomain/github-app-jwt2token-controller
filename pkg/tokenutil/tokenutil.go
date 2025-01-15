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

package tokenutil

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/golang-jwt/jwt/v5"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"
)

func RetrieveTokenGeneric(kubeclientset kubernetes.Interface, namespace, secretName string) (string, time.Time, error) {
	logger := klog.FromContext(context.Background())
	logger.V(4).Info("Retrieving JWT for secret", "namespace", namespace, "secretName", secretName)

	secret, err := kubeclientset.CoreV1().Secrets(namespace).Get(context.TODO(), secretName, metav1.GetOptions{})
	if err != nil {
		return "", time.Time{}, fmt.Errorf("could not retrieve secret: %v", err)
	}
	privateKey, ok := secret.Data["privateKey"]
	appId, ok := secret.Data["appId"]
	installationId, ok := secret.Data["installationId"]
	if !ok {
		return "", time.Time{}, fmt.Errorf("private key not found in secret")
	}
	parsedKey, err := jwt.ParseRSAPrivateKeyFromPEM(privateKey)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("could not parse private key: %v", err)
	}

	now := time.Now()
	claims := jwt.RegisteredClaims{
		IssuedAt:  jwt.NewNumericDate(now),
		ExpiresAt: jwt.NewNumericDate(now.Add(10 * time.Minute)),
		Issuer:    string(appId),
	}

	token := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	tokenString, err := token.SignedString(parsedKey)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("could not sign token: %v", err)
	}

	ghs_token, expiration, err := ExchangeJWTForGitHubToken(tokenString, string(installationId))
	if err != nil {
		return "", time.Time{}, fmt.Errorf("could not exchange JWT for GitHub token: %v", err)
	}

	return ghs_token, expiration, nil
}

func ExchangeJWTForGitHubToken(jwt string, installationId string) (string, time.Time, error) {
	logger := klog.FromContext(context.Background())
	url := fmt.Sprintf("https://api.github.com/app/installations/%s/access_tokens", installationId)
	req, err := http.NewRequest("POST", url, nil)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("Authorization", "Bearer "+jwt)
	req.Header.Set("Accept", "application/vnd.github+json")

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("failed to send request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		return "", time.Time{}, fmt.Errorf("non-200 status code: %d, response: %s", resp.StatusCode, string(body))
	}

	var responseBody struct {
		Token     string `json:"token"`
		ExpiresAt string `json:"expires_at"`
	}
	err = json.NewDecoder(resp.Body).Decode(&responseBody)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("failed to parse response body: %w", err)
	}
	logger.V(4).Info("GitHub token retrieved successfully", responseBody.ExpiresAt)

	expiresAt, err := time.Parse(time.RFC3339, responseBody.ExpiresAt)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("failed to parse expires_at: %w", err)
	}
	return responseBody.Token, expiresAt, nil
}
