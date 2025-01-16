package main

import (
	"context"
	"fmt"
	"time"

	"golang.org/x/time/rate"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"

	v1 "github.com/kharkevich/github-app-jwt2token-controller/pkg/apis/githubappjwt2token/v1"

	clientset "github.com/kharkevich/github-app-jwt2token-controller/pkg/generated/clientset/versioned"
	informers "github.com/kharkevich/github-app-jwt2token-controller/pkg/generated/informers/externalversions/githubappjwt2token/v1"
	listers "github.com/kharkevich/github-app-jwt2token-controller/pkg/generated/listers/githubappjwt2token/v1"
)

type GHSController struct {
	kubeclientset kubernetes.Interface
	clientset     clientset.Interface
	ghsLister     listers.GHSLister
	ghsSynced     cache.InformerSynced
	ctx           context.Context
	workqueue     workqueue.TypedRateLimitingInterface[cache.ObjectName]
}

func NewGHSController(ctx context.Context, kubeclientset kubernetes.Interface, clientset clientset.Interface, ghsInformer informers.GHSInformer) *GHSController {
	ratelimiter := workqueue.NewTypedMaxOfRateLimiter(
		workqueue.NewTypedItemExponentialFailureRateLimiter[cache.ObjectName](5*time.Millisecond, 1000*time.Second),
		&workqueue.TypedBucketRateLimiter[cache.ObjectName]{Limiter: rate.NewLimiter(rate.Limit(50), 300)},
	)

	controller := &GHSController{
		kubeclientset: kubeclientset,
		clientset:     clientset,
		ghsLister:     ghsInformer.Lister(),
		ghsSynced:     ghsInformer.Informer().HasSynced,
		ctx:           ctx,
		workqueue:     workqueue.NewTypedRateLimitingQueue(ratelimiter),
	}

	ghsInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    controller.enqueueGHS,
		UpdateFunc: func(old, new interface{}) { controller.enqueueGHS(new) },
		DeleteFunc: controller.enqueueGHS,
	})

	return controller
}

func (c *GHSController) Run(ctx context.Context, threadiness int) error {
	defer klog.Flush()

	klog.Info("Starting GHS controller")

	if ok := cache.WaitForCacheSync(ctx.Done(), c.ghsSynced); !ok {
		return fmt.Errorf("failed to wait for caches to sync")
	}

	for i := 0; i < threadiness; i++ {
		go wait.UntilWithContext(ctx, c.runWorker, time.Second)
	}

	<-ctx.Done()
	return nil
}

func (c *GHSController) runWorker(ctx context.Context) {
	for c.processNextWorkItem(ctx) {
	}
}

func (c *GHSController) processNextWorkItem(ctx context.Context) bool {
	objRef, shutdown := c.workqueue.Get()
	if shutdown {
		return false
	}

	defer c.workqueue.Done(objRef)

	err := c.syncHandler(ctx, objRef)
	if err == nil {
		c.workqueue.Forget(objRef)
		return true
	}

	utilruntime.HandleErrorWithContext(ctx, err, "Error syncing; requeuing for later retry", "objectReference", objRef)
	c.workqueue.AddRateLimited(objRef)
	return true
}

func (c *GHSController) syncHandler(ctx context.Context, objectRef cache.ObjectName) error {
	logger := klog.LoggerWithValues(klog.FromContext(ctx), "objectRef", objectRef)
	namespace, name := objectRef.Namespace, objectRef.Name

	// Re-fetch the GHS resource to ensure we have the latest status
	ghs, err := c.clientset.GithubappV1().GHSs(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		if errors.IsNotFound(err) {
			utilruntime.HandleErrorWithContext(ctx, fmt.Errorf("GHS '%s/%s' in work queue no longer exists", namespace, name), "namespace", namespace, "name", name)
			return nil
		}
		return err
	}

	// Check if the GHS resource should be deleted
	if shouldDeleteGHS(ghs) {
		logger.V(4).Info("Deleting GHS", "name", ghs.Name)
		return c.clientset.GithubappV1().GHSs(namespace).Delete(ctx, name, metav1.DeleteOptions{})
	}

	return nil
}

func shouldDeleteGHS(ghs *v1.GHS) bool {
	// Delete if it was created more than 24 hours ago
	if ghs.CreationTimestamp.Time.Before(time.Now().Add(-24 * time.Hour)) {
		return true
	}

	// Check if the expiration time is set and has passed
	if !ghs.Status.ExpiresAt.Time.IsZero() && ghs.Status.ExpiresAt.Time.Before(time.Now().Add(+15*time.Minute)) {
		return true
	}

	return false
}

func (c *GHSController) enqueueGHS(obj interface{}) {
	if objectRef, err := cache.ObjectToName(obj); err != nil {
		utilruntime.HandleError(err)
		return
	} else {
		klog.FromContext(context.Background()).V(4).Info("Enqueuing GHS", "objectName", objectRef)
		c.workqueue.Add(objectRef)
	}
}
