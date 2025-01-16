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
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
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

	githubappjwt2tokenv1 "github.com/kharkevich/github-app-jwt2token-controller/pkg/apis/githubappjwt2token/v1"
	clientset "github.com/kharkevich/github-app-jwt2token-controller/pkg/generated/clientset/versioned"
	jwt2tokenAppScheme "github.com/kharkevich/github-app-jwt2token-controller/pkg/generated/clientset/versioned/scheme"
	informers "github.com/kharkevich/github-app-jwt2token-controller/pkg/generated/informers/externalversions/githubappjwt2token/v1"
	listers "github.com/kharkevich/github-app-jwt2token-controller/pkg/generated/listers/githubappjwt2token/v1"
	"github.com/kharkevich/github-app-jwt2token-controller/pkg/ghsutil"
	"github.com/kharkevich/github-app-jwt2token-controller/pkg/tokenutil"
)

type DockerConfigJsonController struct {
	kubeclientset             kubernetes.Interface
	dockerConfigJsonClientset clientset.Interface
	dockerConfigJsonLister    listers.DockerConfigJsonLister
	dockerConfigJsonSynced    cache.InformerSynced
	workqueue                 workqueue.TypedRateLimitingInterface[cache.ObjectName]
	recorder                  record.EventRecorder
}

func NewDockerConfigJsonController(
	ctx context.Context,
	kubeclientset kubernetes.Interface,
	dockerConfigJsonClientset clientset.Interface,
	dockerConfigJsonInformer informers.DockerConfigJsonInformer) *DockerConfigJsonController {
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

	controller := &DockerConfigJsonController{
		kubeclientset:             kubeclientset,
		dockerConfigJsonClientset: dockerConfigJsonClientset,
		dockerConfigJsonLister:    dockerConfigJsonInformer.Lister(),
		dockerConfigJsonSynced:    dockerConfigJsonInformer.Informer().HasSynced,
		workqueue:                 workqueue.NewTypedRateLimitingQueue(ratelimiter),
		recorder:                  recorder,
	}

	logger.V(4).Info("Setting up event handlers")
	dockerConfigJsonInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: controller.enqueueDockerConfigJson,
		UpdateFunc: func(old, new interface{}) {
			controller.enqueueDockerConfigJson(new)
		},
	})

	return controller
}

func (c *DockerConfigJsonController) Run(ctx context.Context, workers int) error {
	defer utilruntime.HandleCrash()
	defer c.workqueue.ShutDown()
	logger := klog.FromContext(ctx)
	klog.Info("Starting DockerConfigJson controller")

	if ok := cache.WaitForCacheSync(ctx.Done(), c.dockerConfigJsonSynced); !ok {
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

func (c *DockerConfigJsonController) runWorker(ctx context.Context) {
	for c.processNextWorkItem(ctx) {
	}
}

func (c *DockerConfigJsonController) processNextWorkItem(ctx context.Context) bool {
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

func (c *DockerConfigJsonController) syncHandler(ctx context.Context, objectRef cache.ObjectName) error {
	logger := klog.LoggerWithValues(klog.FromContext(ctx), "objectRef", objectRef)
	logger.V(4).Info("Syncing DockerConfigJson")

	dockerConfigJson, err := c.dockerConfigJsonLister.DockerConfigJsons(objectRef.Namespace).Get(objectRef.Name)
	if err != nil {
		if errors.IsNotFound(err) {
			utilruntime.HandleErrorWithContext(ctx, err, "DockerConfigJson referenced by item in work queue no longer exists", "objectReference", objectRef)
			return nil
		}
		logger.Error(err, "Failed to get DockerConfigJson")
		return err
	}

	err = c.takeOwnership(ctx, dockerConfigJson)
	if err != nil {
		return err
	}
	if shouldRegenerateToken(ctx, c.dockerConfigJsonClientset, dockerConfigJson.Status.Token, dockerConfigJson.Namespace) {
		token, expiresAt, err := c.retrieveDockerConfigToken(dockerConfigJson)
		if err != nil {
			logger.Error(err, "Failed to retrieve GitHub token for DockerConfigJson", "namespace", dockerConfigJson.Namespace, "name", dockerConfigJson.Name)
			return err
		}

		hash := md5.Sum([]byte(token))
		tokenHash := hex.EncodeToString(hash[:])
		dockerConfigJson.Status.Token = tokenHash

		err = ghsutil.CreateOrUpdateGHS(ctx, c.dockerConfigJsonClientset, tokenHash, token, dockerConfigJson.Namespace, expiresAt)
		if err != nil {
			return err
		}

		err = c.updateDockerConfigJsonStatus(ctx, dockerConfigJson)
		if err != nil {
			return err
		}

		err = c.updateDockerConfigSecret(ctx, dockerConfigJson)
		if err != nil {
			logger.Error(err, "Failed to update DockerConfig Secret")
			return err
		}
		logger.V(4).Info(MessageTokenUpdated)
		c.recorder.Event(dockerConfigJson, corev1.EventTypeNormal, SuccessUpdated, MessageTokenUpdated)
		return nil
	}
	logger.V(4).Info(MessageTokenValid)
	c.recorder.Event(dockerConfigJson, corev1.EventTypeNormal, SuccessSynced, MessageTokenValid)
	return nil
}

func (c *DockerConfigJsonController) isControlledByUs(dockerConfigJson *githubappjwt2tokenv1.DockerConfigJson) bool {
	for _, ref := range dockerConfigJson.OwnerReferences {
		if ref.Controller != nil && *ref.Controller {
			return true
		}
	}
	return false
}

func (c *DockerConfigJsonController) takeOwnership(ctx context.Context, dockerConfigJson *githubappjwt2tokenv1.DockerConfigJson) error {
	logger := klog.FromContext(ctx)
	if !c.isControlledByUs(dockerConfigJson) {
		logger.V(4).Info("Taking ownership of DockerConfigJson", "namespace", dockerConfigJson.Namespace, "name", dockerConfigJson.Name)
		ownerRef := metav1.NewControllerRef(dockerConfigJson, githubappjwt2tokenv1.SchemeGroupVersion.WithKind("DockerConfigJson"))
		dockerConfigJsonCopy := dockerConfigJson.DeepCopy()
		dockerConfigJsonCopy.OwnerReferences = append(dockerConfigJsonCopy.OwnerReferences, *ownerRef)
		dockerConfigJsonCopy, err := c.dockerConfigJsonClientset.GithubappV1().DockerConfigJsons(dockerConfigJson.Namespace).Update(ctx, dockerConfigJsonCopy, metav1.UpdateOptions{})
		if err != nil {
			logger.Error(err, "Failed to take ownership of DockerConfigJson", "namespace", dockerConfigJson.Namespace, "name", dockerConfigJson.Name)
			return err
		}
	}
	return nil
}

func (c *DockerConfigJsonController) updateDockerConfigJsonStatus(ctx context.Context, dockerConfigJson *githubappjwt2tokenv1.DockerConfigJson) error {
	logger := klog.LoggerWithValues(klog.FromContext(ctx), "namespace", dockerConfigJson.Namespace, "name", dockerConfigJson.Name)

	latest, getErr := c.dockerConfigJsonClientset.GithubappV1().DockerConfigJsons(dockerConfigJson.Namespace).Get(ctx, dockerConfigJson.Name, metav1.GetOptions{})
	if errors.IsNotFound(getErr) {
		logger.V(4).Info("DockerConfigJson no longer exists, skipping status update", "namespace", dockerConfigJson.Namespace, "name", dockerConfigJson.Name)
		return nil
	}
	if getErr != nil {
		logger.Error(getErr, "Failed to retrieve latest DockerConfigJson for status update")
		return getErr
	}
	latestCopy := latest.DeepCopy()
	latestCopy.Status = dockerConfigJson.Status
	_, updateErr := c.dockerConfigJsonClientset.GithubappV1().DockerConfigJsons(latest.Namespace).UpdateStatus(ctx, latestCopy, metav1.UpdateOptions{FieldManager: FieldManager})
	if updateErr != nil {
		logger.Error(updateErr, "Failed to update DockerConfigJson status")
	}

	return nil
}

func (c *DockerConfigJsonController) updateDockerConfigSecret(ctx context.Context, dockerConfigJson *githubappjwt2tokenv1.DockerConfigJson) error {
	for _, secretConfig := range dockerConfigJson.Spec.DockerConfigSecrets {
		logger := klog.LoggerWithValues(klog.FromContext(ctx), "secret", secretConfig.Secret, "namespace", secretConfig.Namespace)
		ghs_token, err := ghsutil.GetGHS(ctx, c.dockerConfigJsonClientset, dockerConfigJson.Status.Token, dockerConfigJson.Namespace)
		if err != nil {
			logger.Error(err, "Failed to retrieve GHS")
			return err
		}

		auth := base64.StdEncoding.EncodeToString([]byte(fmt.Sprintf("%s:%s", secretConfig.Username, ghs_token.Spec.Token)))
		dockerConfigJsonData, err := json.Marshal(map[string]interface{}{
			"auths": map[string]interface{}{
				secretConfig.Registry: map[string]string{
					"auth": auth,
				},
			},
		})
		if err != nil {
			return err
		}

		logger.V(4).Info("Updating DockerConfig Secret")

		secret, err := c.kubeclientset.CoreV1().Secrets(secretConfig.Namespace).Get(ctx, secretConfig.Secret, metav1.GetOptions{})
		if errors.IsNotFound(err) {
			logger.V(4).Info("Secret not found, creating a new one")
			newSecret := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:      secretConfig.Secret,
					Namespace: secretConfig.Namespace,
				},
				Type: corev1.SecretTypeDockerConfigJson,
				Data: map[string][]byte{
					".dockerconfigjson": dockerConfigJsonData,
				},
			}
			_, err = c.kubeclientset.CoreV1().Secrets(secretConfig.Namespace).Create(ctx, newSecret, metav1.CreateOptions{})
			if err != nil {
				logger.Error(err, "Failed to create Secret")
				return err
			}
			logger.V(4).Info("DockerConfig Secret created successfully", "secret", secretConfig.Secret, "namespace", secretConfig.Namespace)
			continue
		} else if err != nil {
			logger.Error(err, "Failed to retrieve Secret")
			return err
		}

		if !bytes.Equal(secret.Data[".dockerconfigjson"], dockerConfigJsonData) {
			secretCopy := secret.DeepCopy()
			secretCopy.Data[".dockerconfigjson"] = dockerConfigJsonData
			_, err = c.kubeclientset.CoreV1().Secrets(secretConfig.Namespace).Update(ctx, secretCopy, metav1.UpdateOptions{})
			if err != nil {
				return err
			}
		}
		logger.V(4).Info("DockerConfig Secret updated successfully", "secret", secretConfig.Secret, "namespace", secretConfig.Namespace)
	}

	return nil
}

func (c *DockerConfigJsonController) retrieveDockerConfigToken(dockerConfigJson *githubappjwt2tokenv1.DockerConfigJson) (string, time.Time, error) {
	return tokenutil.RetrieveTokenGeneric(c.kubeclientset, dockerConfigJson.Namespace, dockerConfigJson.Spec.PrivateKeySecret)
}

func (c *DockerConfigJsonController) enqueueDockerConfigJson(obj interface{}) {
	if objectRef, err := cache.ObjectToName(obj); err != nil {
		utilruntime.HandleError(err)
		return
	} else {
		klog.FromContext(context.Background()).V(4).Info("Enqueuing DockerConfigJson", "objectName", objectRef)
		c.workqueue.Add(objectRef)
	}
}
