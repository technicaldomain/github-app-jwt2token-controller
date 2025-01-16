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

type ArgoCDRepoController struct {
	kubeclientset       kubernetes.Interface
	argoCDRepoClientset clientset.Interface
	argoCDRepoLister    listers.ArgoCDRepoLister
	argoCDRepoSynced    cache.InformerSynced
	workqueue           workqueue.TypedRateLimitingInterface[cache.ObjectName]
	recorder            record.EventRecorder
}

func NewArgoCDRepoController(
	ctx context.Context,
	kubeclientset kubernetes.Interface,
	argoCDRepoClientset clientset.Interface,
	argoCDRepoInformer informers.ArgoCDRepoInformer) *ArgoCDRepoController {
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

	controller := &ArgoCDRepoController{
		kubeclientset:       kubeclientset,
		argoCDRepoClientset: argoCDRepoClientset,
		argoCDRepoLister:    argoCDRepoInformer.Lister(),
		argoCDRepoSynced:    argoCDRepoInformer.Informer().HasSynced,
		workqueue:           workqueue.NewTypedRateLimitingQueue(ratelimiter),
		recorder:            recorder,
	}

	logger.V(4).Info("Setting up event handlers")
	argoCDRepoInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: controller.enqueueArgoCDRepo,
		UpdateFunc: func(old, new interface{}) {
			controller.enqueueArgoCDRepo(new)
		},
	})

	return controller
}

func (c *ArgoCDRepoController) Run(ctx context.Context, workers int) error {
	defer utilruntime.HandleCrash()
	defer c.workqueue.ShutDown()
	logger := klog.FromContext(ctx)
	klog.Info("Starting ArgoCDRepo controller")

	if ok := cache.WaitForCacheSync(ctx.Done(), c.argoCDRepoSynced); !ok {
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

func (c *ArgoCDRepoController) runWorker(ctx context.Context) {
	for c.processNextWorkItem(ctx) {
	}
}

func (c *ArgoCDRepoController) processNextWorkItem(ctx context.Context) bool {
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

func (c *ArgoCDRepoController) syncHandler(ctx context.Context, objectRef cache.ObjectName) error {
	logger := klog.LoggerWithValues(klog.FromContext(ctx), "objectRef", objectRef)
	logger.V(4).Info("Syncing ArgoCDRepo")

	argoCDRepo, err := c.argoCDRepoLister.ArgoCDRepos(objectRef.Namespace).Get(objectRef.Name)
	if err != nil {
		if errors.IsNotFound(err) {
			utilruntime.HandleErrorWithContext(ctx, err, "ArgoCDRepo referenced by item in work queue no longer exists", "objectReference", objectRef)
			return nil
		}
		logger.Error(err, "Failed to get ArgoCDRepo")
		return err
	}

	err = c.takeOwnership(ctx, argoCDRepo)
	if err != nil {
		return err
	}

	if shouldRegenerateToken(ctx, c.argoCDRepoClientset, argoCDRepo.Status.Token, argoCDRepo.Namespace) {
		token, expiresAt, err := c.retrieveArgoCDRepoToken(argoCDRepo)
		if err != nil {
			logger.Error(err, "Failed to retrieve GitHub token for ArgoCDRepo", "namespace", argoCDRepo.Namespace, "name", argoCDRepo.Name)
			return err
		}

		hash := md5.Sum([]byte(token))
		tokenHash := hex.EncodeToString(hash[:])
		argoCDRepo.Status.Token = tokenHash

		err = ghsutil.CreateOrUpdateGHS(ctx, c.argoCDRepoClientset, tokenHash, token, argoCDRepo.Namespace, expiresAt)
		if err != nil {
			return err
		}

		err = c.updateArgoCDRepoStatus(ctx, argoCDRepo)
		if err != nil {
			return err
		}

		err = c.updateArgoCDSecret(ctx, argoCDRepo)
		if err != nil {
			logger.Error(err, "Failed to update ArgoCD Secret")
			return err
		}
		logger.V(4).Info(MessageTokenUpdated)
		c.recorder.Event(argoCDRepo, corev1.EventTypeNormal, SuccessUpdated, MessageTokenUpdated)
		return nil
	}
	logger.V(4).Info(MessageTokenValid)
	return nil
}

func (c *ArgoCDRepoController) takeOwnership(ctx context.Context, argoCDRepo *githubappjwt2tokenv1.ArgoCDRepo) error {
	logger := klog.FromContext(ctx)
	if !c.isControlledByUs(argoCDRepo) {
		logger.V(4).Info("Taking ownership of ArgoCDRepo")
		ownerRef := metav1.NewControllerRef(argoCDRepo, githubappjwt2tokenv1.SchemeGroupVersion.WithKind("ArgoCDRepo"))
		argoCDRepoCopy := argoCDRepo.DeepCopy()
		argoCDRepoCopy.OwnerReferences = append(argoCDRepoCopy.OwnerReferences, *ownerRef)
		argoCDRepoCopy, err := c.argoCDRepoClientset.GithubappV1().ArgoCDRepos(argoCDRepo.Namespace).Update(ctx, argoCDRepoCopy, metav1.UpdateOptions{})
		if err != nil {
			logger.Error(err, "Failed to take ownership of ArgoCDRepo", "namespace", argoCDRepo.Namespace, "name", argoCDRepo.Name)
			return err
		}
	}
	return nil
}

func (c *ArgoCDRepoController) updateArgoCDRepoStatus(ctx context.Context, argoCDRepo *githubappjwt2tokenv1.ArgoCDRepo) error {
	// NEVER modify objects from the store. It's a read-only, local cache.
	// You can use DeepCopy() to make a deep copy of original object and modify this copy
	// Or create a copy manually for better performance

	// Log the current status before updating
	logger := klog.LoggerWithValues(klog.FromContext(ctx), "namespace", argoCDRepo.Namespace, "name", argoCDRepo.Name)

	latest, getErr := c.argoCDRepoClientset.GithubappV1().ArgoCDRepos(argoCDRepo.Namespace).Get(ctx, argoCDRepo.Name, metav1.GetOptions{})
	if errors.IsNotFound(getErr) {
		logger.V(4).Info("ArgoCDRepo no longer exists, skipping status update", "namespace", argoCDRepo.Namespace, "name", argoCDRepo.Name)
		return nil
	}
	if getErr != nil {
		logger.Error(getErr, "Failed to retrieve latest ArgoCDRepo for status update")
		return getErr
	}
	latestCopy := latest.DeepCopy()
	latestCopy.Status = argoCDRepo.Status
	_, updateErr := c.argoCDRepoClientset.GithubappV1().ArgoCDRepos(latest.Namespace).UpdateStatus(ctx, latestCopy, metav1.UpdateOptions{FieldManager: FieldManager})
	if updateErr != nil {
		logger.Error(updateErr, "Failed to update ArgoCDRepo status")
	}

	return nil
}

func (c *ArgoCDRepoController) isControlledByUs(argoCDRepo *githubappjwt2tokenv1.ArgoCDRepo) bool {
	for _, ownerRef := range argoCDRepo.OwnerReferences {
		if ownerRef.Controller != nil && *ownerRef.Controller {
			return true
		}
	}
	return false
}

func (c *ArgoCDRepoController) retrieveArgoCDRepoToken(argoCDRepo *githubappjwt2tokenv1.ArgoCDRepo) (string, time.Time, error) {
	return tokenutil.RetrieveTokenGeneric(c.kubeclientset, argoCDRepo.Namespace, argoCDRepo.Spec.PrivateKeySecret)
}

func (c *ArgoCDRepoController) updateArgoCDSecret(ctx context.Context, argoCDRepo *githubappjwt2tokenv1.ArgoCDRepo) error {
	for _, repo := range argoCDRepo.Spec.ArgoCDRepositories {
		logger := klog.LoggerWithValues(klog.FromContext(ctx), "repository", repo.Repository, "namespace", repo.Namespace)
		logger.V(4).Info("Updating ArgoCD Secret")

		secret, err := c.kubeclientset.CoreV1().Secrets(repo.Namespace).Get(ctx, repo.Repository, metav1.GetOptions{})
		if err != nil {
			logger.Error(err, "Failed to retrieve Secret")
			return err
		}

		ghs_token, err := ghsutil.GetGHS(ctx, c.argoCDRepoClientset, argoCDRepo.Status.Token, argoCDRepo.Namespace)
		if err != nil {
			logger.Error(err, "Failed to retrieve GHS")
			return err
		}

		if !bytes.Equal(secret.Data["password"], []byte(ghs_token.Spec.Token)) {
			secretCopy := secret.DeepCopy()
			secretCopy.Data["password"] = []byte(ghs_token.Spec.Token)
			_, err = c.kubeclientset.CoreV1().Secrets(repo.Namespace).Update(ctx, secretCopy, metav1.UpdateOptions{})
			if err != nil {
				return err
			}
		}

		logger.V(4).Info("ArgoCD Secret updated successfully", "repository", repo.Repository, "namespace", repo.Namespace)
	}

	return nil
}

func (c *ArgoCDRepoController) enqueueArgoCDRepo(obj interface{}) {
	if objectRef, err := cache.ObjectToName(obj); err != nil {
		utilruntime.HandleError(err)
		return
	} else {
		klog.FromContext(context.Background()).V(4).Info("Enqueuing ArgoCDRepo", "objectName", objectRef)
		c.workqueue.Add(objectRef)
	}
}
