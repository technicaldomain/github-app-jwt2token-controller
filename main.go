/*
Copyright 2017 The Kubernetes Authors.

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
	"flag"
	"time"

	"github.com/kharkevich/github-app-jwt2token-controller/pkg/signals"
	kubeinformers "k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/klog/v2"

	clientset "github.com/kharkevich/github-app-jwt2token-controller/pkg/generated/clientset/versioned"
	informers "github.com/kharkevich/github-app-jwt2token-controller/pkg/generated/informers/externalversions"
)

var (
	masterURL  string
	kubeconfig string
)

func main() {
	klog.InitFlags(nil)
	flag.Parse()

	// set up signals so we handle the shutdown signal gracefully
	ctx := signals.SetupSignalHandler()
	logger := klog.FromContext(ctx)

	cfg, err := clientcmd.BuildConfigFromFlags(masterURL, kubeconfig)
	if err != nil {
		logger.Error(err, "Error building kubeconfig")
		klog.FlushAndExit(klog.ExitFlushTimeout, 1)
	}

	kubeClient, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		logger.Error(err, "Error building kubernetes clientset")
		klog.FlushAndExit(klog.ExitFlushTimeout, 1)
	}

	gitHubAppJWT2TokenClient, err := clientset.NewForConfig(cfg)
	if err != nil {
		logger.Error(err, "Error building kubernetes clientset")
		klog.FlushAndExit(klog.ExitFlushTimeout, 1)
	}

	kubeInformerFactory := kubeinformers.NewSharedInformerFactory(kubeClient, time.Second*30)
	githubAppInformerFactory := informers.NewSharedInformerFactory(gitHubAppJWT2TokenClient, time.Second*30)

	argoCDRepoController := NewArgoCDRepoController(ctx, kubeClient, gitHubAppJWT2TokenClient,
		githubAppInformerFactory.Githubapp().V1().ArgoCDRepos())

	dockerConfigJsonController := NewDockerConfigJsonController(ctx, kubeClient, gitHubAppJWT2TokenClient,
		githubAppInformerFactory.Githubapp().V1().DockerConfigJsons())

	ghsController := NewGHSController(ctx, kubeClient, gitHubAppJWT2TokenClient,
		githubAppInformerFactory.Githubapp().V1().GHSs())

	// notice that there is no need to run Start methods in a separate goroutine. (i.e. go kubeInformerFactory.Start(ctx.done())
	// Start method is non-blocking and runs all registered informers in a dedicated goroutine.
	kubeInformerFactory.Start(ctx.Done())
	githubAppInformerFactory.Start(ctx.Done())

	go func() {
		if err = argoCDRepoController.Run(ctx, 2); err != nil {
			logger.Error(err, "Error running ArgoCDRepo controller")
			klog.FlushAndExit(klog.ExitFlushTimeout, 1)
		}
	}()

	go func() {
		if err = dockerConfigJsonController.Run(ctx, 2); err != nil {
			logger.Error(err, "Error running DockerConfigJson controller")
			klog.FlushAndExit(klog.ExitFlushTimeout, 1)
		}
	}()

	go func() {
		if err = ghsController.Run(ctx, 2); err != nil {
			logger.Error(err, "Error running GHS controller")
			klog.FlushAndExit(klog.ExitFlushTimeout, 1)
		}
	}()

	<-ctx.Done()
}

func init() {
	flag.StringVar(&kubeconfig, "kubeconfig", "", "Path to a kubeconfig. Only required if out-of-cluster.")
	flag.StringVar(&masterURL, "master", "", "The address of the Kubernetes API server. Overrides any value in kubeconfig. Only required if out-of-cluster.")
}
