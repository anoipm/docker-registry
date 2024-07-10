/*
Copyright 2022.

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
	"flag"
	"os"
	"time"

	// Import all Kubernetes client auth plugins (e.g. Azure, GCP, OIDC, etc.)
	// to ensure that exec-entrypoint and run can make use of them.
	"github.com/pkg/errors"
	uberzap "go.uber.org/zap"
	uberzapcore "go.uber.org/zap/zapcore"
	istionetworking "istio.io/client-go/pkg/apis/networking/v1beta1"
	corev1 "k8s.io/api/core/v1"
	apiextensionsscheme "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset/scheme"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	_ "k8s.io/client-go/plugin/pkg/client/auth"
	ctrl "sigs.k8s.io/controller-runtime"
	ctrlcache "sigs.k8s.io/controller-runtime/pkg/cache"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	operatorv1alpha1 "github.com/kyma-project/docker-registry/components/operator/api/v1alpha1"
	"github.com/kyma-project/docker-registry/components/operator/controllers"
	"github.com/kyma-project/docker-registry/components/operator/internal/config"
	k8s "github.com/kyma-project/docker-registry/components/operator/internal/controllers/kubernetes"
	"github.com/kyma-project/docker-registry/components/operator/internal/gitrepository"
	"github.com/kyma-project/docker-registry/components/operator/internal/registry"
	internalresource "github.com/kyma-project/docker-registry/components/operator/internal/resource"
	//+kubebuilder:scaffold:imports
)

var (
	scheme         = runtime.NewScheme()
	setupLog       = ctrl.Log.WithName("setup")
	syncPeriod     = time.Minute * 30
	cleanupTimeout = time.Second * 10
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))

	utilruntime.Must(operatorv1alpha1.AddToScheme(scheme))

	utilruntime.Must(apiextensionsscheme.AddToScheme(scheme))

	utilruntime.Must(istionetworking.AddToScheme(scheme))

	//+kubebuilder:scaffold:scheme
}

func main() {
	var metricsAddr string
	var probeAddr string
	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080", "The address the metric endpoint binds to.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "The address the probe endpoint binds to.")

	opts := zap.Options{
		Development: true,
		TimeEncoder: uberzapcore.TimeEncoderOfLayout("Jan 02 15:04:05.000000000"),
	}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	cfg, err := config.GetConfig("")
	if err != nil {
		setupLog.Error(err, "while getting config")
		os.Exit(1)
	}

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	ctx, cancel := context.WithTimeout(context.Background(), cleanupTimeout)
	defer cancel()

	setupLog.Info("cleaning orphan deprecated resources")
	err = cleanupOrphanDeprecatedResources(ctx)
	if err != nil {
		setupLog.Error(err, "while removing orphan resources")
		os.Exit(1)
	}

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme: scheme,
		Metrics: ctrlmetrics.Options{
			BindAddress: metricsAddr,
		},
		HealthProbeBindAddress: probeAddr,
		Cache: ctrlcache.Options{
			SyncPeriod: &syncPeriod,
		},
		Client: ctrlclient.Options{
			Cache: &ctrlclient.CacheOptions{
				DisableFor: []ctrlclient.Object{
					&corev1.Secret{},
					&corev1.ConfigMap{},
				},
			},
		},
		// TODO: use our own logger - now eventing use logger with different message format
	})
	if err != nil {
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}

	config := uberzap.NewDevelopmentConfig()
	config.EncoderConfig.TimeKey = "timestamp"
	config.EncoderConfig.EncodeTime = opts.TimeEncoder
	config.DisableCaller = true

	reconcilerLogger, err := config.Build()
	if err != nil {
		setupLog.Error(err, "unable to setup logger")
		os.Exit(1)
	}

	reconciler := controllers.NewDockerRegistryReconciler(
		mgr.GetClient(), mgr.GetConfig(),
		mgr.GetEventRecorderFor("dockerregistry-operator"),
		reconcilerLogger.Sugar(),
		cfg.ChartPath)

	//TODO: get it from some configuration
	configKubernetes := k8s.Config{
		BaseNamespace:                 "kyma-system",
		BaseInternalSecretName:        registry.InternalAccessSecretName,
		BaseExternalSecretName:        registry.ExternalAccessSecretName,
		ExcludedNamespaces:            []string{"kyma-system"},
		ConfigMapRequeueDuration:      time.Minute,
		SecretRequeueDuration:         time.Minute,
		ServiceAccountRequeueDuration: time.Minute,
	}

	resourceClient := internalresource.New(mgr.GetClient(), scheme)
	secretSvc := k8s.NewSecretService(resourceClient, configKubernetes)

	if err = reconciler.SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "DockerRegistry")
		os.Exit(1)
	}

	namespaceLogger, err := config.Build()
	if err != nil {
		setupLog.Error(err, "unable to setup logger")
		os.Exit(1)
	}

	if err := k8s.NewNamespace(mgr.GetClient(), namespaceLogger.Sugar(), configKubernetes, secretSvc).
		SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create Namespace controller")
		os.Exit(1)
	}

	secretLogger, err := config.Build()
	if err != nil {
		setupLog.Error(err, "unable to setup logger")
		os.Exit(1)
	}

	if err := k8s.NewSecret(mgr.GetClient(), secretLogger.Sugar(), configKubernetes, secretSvc).
		SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create Secret controller")
		os.Exit(1)
	}
	//+kubebuilder:scaffold:builder

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up ready check")
		os.Exit(1)
	}

	setupLog.Info("starting manager")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "problem running manager")
		os.Exit(1)
	}
}

func cleanupOrphanDeprecatedResources(ctx context.Context) error {
	// We are going to talk to the API server _before_ we start the manager.
	// Since the default manager client reads from cache, we will get an error.
	// So, we create a "serverClient" that would read from the API directly.
	// We only use it here, this only runs at start up, so it shouldn't be to much for the API
	serverClient, err := ctrlclient.New(ctrl.GetConfigOrDie(), ctrlclient.Options{
		Scheme: scheme,
	})
	if err != nil {
		return errors.Wrap(err, "failed to create a server client")
	}

	return gitrepository.Cleanup(ctx, serverClient)
}
