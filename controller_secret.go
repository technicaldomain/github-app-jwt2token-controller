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
	"bytes"
	"context"
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"time"

	"golang.org/x/time/rate"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	typedcorev1 "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"

	githubappjwt2tokenv1 "github.com/technicaldomain/github-app-jwt2token-controller/pkg/apis/githubappjwt2token/v1"
	clientset "github.com/technicaldomain/github-app-jwt2token-controller/pkg/generated/clientset/versioned"
	jwt2tokenAppScheme "github.com/technicaldomain/github-app-jwt2token-controller/pkg/generated/clientset/versioned/scheme"
	informers "github.com/technicaldomain/github-app-jwt2token-controller/pkg/generated/informers/externalversions/githubappjwt2token/v1"
	listers "github.com/technicaldomain/github-app-jwt2token-controller/pkg/generated/listers/githubappjwt2token/v1"
	"github.com/technicaldomain/github-app-jwt2token-controller/pkg/ghsutil"
	"github.com/technicaldomain/github-app-jwt2token-controller/pkg/tokenutil"
)

type SecretController struct {
	kubeclientset   kubernetes.Interface
	secretClientset clientset.Interface
	secretLister    listers.SecretLister
	secretSynced    cache.InformerSynced
	workqueue       workqueue.TypedRateLimitingInterface[cache.ObjectName]
	recorder        record.EventRecorder
}

func NewSecretController(
	ctx context.Context,
	kubeclientset kubernetes.Interface,
	secretClientset clientset.Interface,
	secretInformer informers.SecretInformer) *SecretController {
	logger := klog.FromContext(ctx)

	utilruntime.Must(jwt2tokenAppScheme.AddToScheme(scheme.Scheme))
	logger.V(4).Info("Creating event broadcaster")

	eventBroadcaster := record.NewBroadcaster(record.WithContext(ctx))
	eventBroadcaster.StartStructuredLogging(0)
	eventBroadcaster.StartRecordingToSink(&typedcorev1.EventSinkImpl{Interface: kubeclientset.CoreV1().Events("")})
	recorder := eventBroadcaster.NewRecorder(scheme.Scheme, corev1.EventSource{Component: controllerAgentName})
	ratelimiter := workqueue.NewTypedMaxOfRateLimiter(
		workqueue.NewTypedItemExponentialFailureRateLimiter[cache.ObjectName](5*time.Millisecond, 1000*time.Second),
		&workqueue.TypedBucketRateLimiter[cache.ObjectName]{Limiter: rate.NewLimiter(rate.Limit(50), 300)},
	)

	controller := &SecretController{
		kubeclientset:   kubeclientset,
		secretClientset: secretClientset,
		secretLister:    secretInformer.Lister(),
		secretSynced:    secretInformer.Informer().HasSynced,
		workqueue:       workqueue.NewTypedRateLimitingQueue(ratelimiter),
		recorder:        recorder,
	}

	logger.V(4).Info("Setting up event handlers")
	secretInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: controller.enqueueSecret,
		UpdateFunc: func(old, new interface{}) {
			controller.enqueueSecret(new)
		},
	})

	return controller
}

func (c *SecretController) Run(ctx context.Context, workers int) error {
	defer utilruntime.HandleCrash()
	defer c.workqueue.ShutDown()
	logger := klog.FromContext(ctx)
	klog.Info("Starting Secret controller")

	if ok := cache.WaitForCacheSync(ctx.Done(), c.secretSynced); !ok {
		return fmt.Errorf("failed to wait for caches to sync")
	}

	logger.V(4).Info("Starting workers", "count", workers)
	for i := 0; i < workers; i++ {
		go wait.UntilWithContext(ctx, c.runWorker, time.Second)
	}

	logger.V(4).Info("Started workers")
	<-ctx.Done()
	logger.V(4).Info("Shutting down workers")

	return nil
}

func (c *SecretController) runWorker(ctx context.Context) {
	for c.processNextWorkItem(ctx) {
	}
}

func (c *SecretController) processNextWorkItem(ctx context.Context) bool {
	objRef, shutdown := c.workqueue.Get()
	logger := klog.FromContext(ctx)

	if shutdown {
		logger.V(4).Info("Workqueue is shutting down")
		return false
	}

	defer c.workqueue.Done(objRef)
	err := c.syncHandler(ctx, objRef)
	if err == nil {
		c.workqueue.Forget(objRef)
		logger.V(4).Info("Successfully synced", "objectName", objRef)
		return true
	}

	utilruntime.HandleErrorWithContext(ctx, err, "Error syncing; requeuing for later retry", "objectReference", objRef)
	c.workqueue.AddRateLimited(objRef)
	return true
}

func (c *SecretController) syncHandler(ctx context.Context, objectRef cache.ObjectName) error {
	logger := klog.LoggerWithValues(klog.FromContext(ctx), "objectRef", objectRef)
	logger.V(4).Info("Syncing Secret")

	secret, err := c.secretLister.Secrets(objectRef.Namespace).Get(objectRef.Name)
	if err != nil {
		if errors.IsNotFound(err) {
			utilruntime.HandleErrorWithContext(ctx, err, "Secret referenced by item in work queue no longer exists", "objectReference", objectRef)
			return nil
		}
		logger.Error(err, "Failed to get Secret")
		return err
	}

	err = c.takeOwnership(ctx, secret)
	if err != nil {
		return err
	}
	if shouldRegenerateToken(ctx, c.secretClientset, secret.Status.Token, secret.Namespace) {
		token, expiresAt, err := c.retrieveSecretToken(secret)
		if err != nil {
			logger.Error(err, "Failed to retrieve GitHub token for Secret", "namespace", secret.Namespace, "name", secret.Name)
			return err
		}

		hash := md5.Sum([]byte(token))
		tokenHash := hex.EncodeToString(hash[:])
		secret.Status.Token = tokenHash

		err = ghsutil.CreateOrUpdateGHS(ctx, c.secretClientset, tokenHash, token, secret.Namespace, expiresAt)
		if err != nil {
			return err
		}

		err = c.updateSecretStatus(ctx, secret)
		if err != nil {
			return err
		}

		logger.V(4).Info(MessageTokenUpdated)
		c.recorder.Event(secret, corev1.EventTypeNormal, SuccessUpdated, MessageTokenUpdated)
		return nil
	}
	logger.V(4).Info(MessageTokenValid)

	err = c.updateSecret(ctx, secret)
	if err != nil {
		logger.Error(err, "Failed to update Secret")
		return err
	}

	return nil
}

func (c *SecretController) isControlledByUs(secret *githubappjwt2tokenv1.Secret) bool {
	for _, ref := range secret.OwnerReferences {
		if ref.Controller != nil && *ref.Controller {
			return true
		}
	}
	return false
}

func (c *SecretController) takeOwnership(ctx context.Context, secret *githubappjwt2tokenv1.Secret) error {
	logger := klog.FromContext(ctx)
	if !c.isControlledByUs(secret) {
		logger.V(4).Info("Taking ownership of Secret", "namespace", secret.Namespace, "name", secret.Name)
		ownerRef := metav1.NewControllerRef(secret, githubappjwt2tokenv1.SchemeGroupVersion.WithKind("Secret"))
		secretCopy := secret.DeepCopy()
		secretCopy.OwnerReferences = append(secretCopy.OwnerReferences, *ownerRef)
		secretCopy, err := c.secretClientset.GithubappV1().Secrets(secret.Namespace).Update(ctx, secretCopy, metav1.UpdateOptions{})
		if err != nil {
			logger.Error(err, "Failed to take ownership of Secret", "namespace", secret.Namespace, "name", secret.Name)
			return err
		}
	}
	return nil
}

func (c *SecretController) updateSecretStatus(ctx context.Context, secret *githubappjwt2tokenv1.Secret) error {
	logger := klog.LoggerWithValues(klog.FromContext(ctx), "namespace", secret.Namespace, "name", secret.Name)

	latest, getErr := c.secretClientset.GithubappV1().Secrets(secret.Namespace).Get(ctx, secret.Name, metav1.GetOptions{})
	if errors.IsNotFound(getErr) {
		logger.V(4).Info("Secret no longer exists, skipping status update", "namespace", secret.Namespace, "name", secret.Name)
		return nil
	}
	if getErr != nil {
		logger.Error(getErr, "Failed to retrieve latest Secret for status update")
		return getErr
	}
	latestCopy := latest.DeepCopy()
	latestCopy.Status = secret.Status
	_, updateErr := c.secretClientset.GithubappV1().Secrets(latest.Namespace).UpdateStatus(ctx, latestCopy, metav1.UpdateOptions{FieldManager: FieldManager})
	if updateErr != nil {
		logger.Error(updateErr, "Failed to update Secret status")
	}

	return nil
}

func (c *SecretController) retrieveSecretToken(secret *githubappjwt2tokenv1.Secret) (string, time.Time, error) {
	return tokenutil.RetrieveTokenGeneric(c.kubeclientset, secret.Namespace, secret.Spec.PrivateKeySecret)
}

func (c *SecretController) updateSecret(ctx context.Context, secret *githubappjwt2tokenv1.Secret) error {
	logger := klog.LoggerWithValues(klog.FromContext(ctx), "namespace", secret.Namespace, "name", secret.Name)
	ghs_token, err := ghsutil.GetGHS(ctx, c.secretClientset, secret.Status.Token, secret.Namespace)
	if err != nil {
		logger.Error(err, "Failed to retrieve GHS")
		return err
	}

	secretData := map[string][]byte{
		"token": []byte(ghs_token.Spec.Token),
	}

	for _, secretConfig := range secret.Spec.Secrets {
		logger := klog.LoggerWithValues(klog.FromContext(ctx), "secret", secretConfig.Secret, "namespace", secretConfig.Namespace)
		logger.V(4).Info("Updating Secret")

		existingSecret, err := c.kubeclientset.CoreV1().Secrets(secretConfig.Namespace).Get(ctx, secretConfig.Secret, metav1.GetOptions{})
		if errors.IsNotFound(err) {
			logger.V(4).Info("Secret not found, creating a new one")
			newSecret := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:      secretConfig.Secret,
					Namespace: secretConfig.Namespace,
				},
				Data: secretData,
			}
			_, err = c.kubeclientset.CoreV1().Secrets(secretConfig.Namespace).Create(ctx, newSecret, metav1.CreateOptions{})
			if err != nil {
				logger.Error(err, "Failed to create Secret")
				return err
			}
			logger.V(4).Info("Secret created successfully", "secret", secretConfig.Secret, "namespace", secretConfig.Namespace)
			continue
		} else if err != nil {
			logger.Error(err, "Failed to retrieve Secret")
			return err
		}

		if !bytes.Equal(existingSecret.Data["token"], secretData["token"]) {
			existingSecretCopy := existingSecret.DeepCopy()
			existingSecretCopy.Data["token"] = secretData["token"]
			_, err = c.kubeclientset.CoreV1().Secrets(secretConfig.Namespace).Update(ctx, existingSecretCopy, metav1.UpdateOptions{})
			if err != nil {
				return err
			}
		}
		logger.V(4).Info("Secret updated successfully", "secret", secretConfig.Secret, "namespace", secretConfig.Namespace)
	}

	return nil
}

func (c *SecretController) enqueueSecret(obj interface{}) {
	if objectRef, err := cache.ObjectToName(obj); err != nil {
		utilruntime.HandleError(err)
		return
	} else {
		klog.FromContext(context.Background()).V(4).Info("Enqueuing Secret", "objectName", objectRef)
		c.workqueue.Add(objectRef)
	}
}
