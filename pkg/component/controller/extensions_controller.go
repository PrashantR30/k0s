/*
Copyright 2022 k0s authors

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

package controller

import (
	"bytes"
	"context"
	"fmt"
	"time"

	"github.com/avast/retry-go"
	"github.com/bombsimon/logrusr/v2"
	"github.com/k0sproject/k0s/internal/pkg/templatewriter"
	"github.com/k0sproject/k0s/pkg/apis/helm.k0sproject.io/v1beta1"
	k0sAPI "github.com/k0sproject/k0s/pkg/apis/k0s.k0sproject.io/v1beta1"
	"github.com/k0sproject/k0s/pkg/component/controller/leaderelector"
	"github.com/k0sproject/k0s/pkg/component/manager"
	"github.com/k0sproject/k0s/pkg/constant"
	"github.com/k0sproject/k0s/pkg/helm"
	kubeutil "github.com/k0sproject/k0s/pkg/kubernetes"
	"github.com/sirupsen/logrus"
	"helm.sh/helm/v3/pkg/release"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/clientcmd"
	apiretry "k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	ctrlManager "sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/yaml"
)

// Helm watch for Chart crd
type ExtensionsController struct {
	saver         manifestsSaver
	L             *logrus.Entry
	helm          *helm.Commands
	kubeConfig    string
	leaderElector leaderelector.Interface
}

var _ manager.Component = (*ExtensionsController)(nil)
var _ manager.Reconciler = (*ExtensionsController)(nil)

// NewExtensionsController builds new HelmAddons
func NewExtensionsController(s manifestsSaver, k0sVars constant.CfgVars, kubeClientFactory kubeutil.ClientFactoryInterface, leaderElector leaderelector.Interface) *ExtensionsController {
	return &ExtensionsController{
		saver:         s,
		L:             logrus.WithFields(logrus.Fields{"component": "extensions_controller"}),
		helm:          helm.NewCommands(k0sVars),
		kubeConfig:    k0sVars.AdminKubeConfigPath,
		leaderElector: leaderElector,
	}
}

const (
	namespaceToWatch = "kube-system"
)

// Run runs the extensions controller
func (ec *ExtensionsController) Reconcile(ctx context.Context, clusterConfig *k0sAPI.ClusterConfig) error {
	ec.L.Info("Extensions reconciliation started")
	defer ec.L.Info("Extensions reconciliation finished")

	helmSettings := clusterConfig.Spec.Extensions.Helm
	var err error
	switch clusterConfig.Spec.Extensions.Storage.Type {
	case k0sAPI.OpenEBSLocal:
		helmSettings, err = addOpenEBSHelmExtension(helmSettings, clusterConfig.Spec.Extensions.Storage)
		if err != nil {
			ec.L.WithError(err).Error("Can't add openebs helm extension")
		}
	default:
	}

	if err := ec.reconcileHelmExtensions(helmSettings); err != nil {
		return fmt.Errorf("can't reconcile helm based extensions: %w", err)
	}

	return nil
}

func addOpenEBSHelmExtension(helmSpec *k0sAPI.HelmExtensions, storageExtension *k0sAPI.StorageExtension) (*k0sAPI.HelmExtensions, error) {
	openEBSValues := map[string]interface{}{
		"localprovisioner": map[string]interface{}{
			"hostpathClass": map[string]interface{}{
				"enabled":        true,
				"isDefaultClass": storageExtension.CreateDefaultStorageClass,
			},
		},
	}
	values, err := yamlifyValues(openEBSValues)
	if err != nil {
		logrus.Errorf("can't yamlify openebs values: %v", err)
		return nil, err
	}
	if helmSpec == nil {
		helmSpec = &k0sAPI.HelmExtensions{
			Repositories: k0sAPI.RepositoriesSettings{},
			Charts:       k0sAPI.ChartsSettings{},
		}
	}
	helmSpec.Repositories = append(helmSpec.Repositories, k0sAPI.Repository{
		Name: "openebs-internal",
		URL:  constant.OpenEBSRepository,
	})
	helmSpec.Charts = append(helmSpec.Charts, k0sAPI.Chart{
		Name:      "openebs",
		ChartName: "openebs-internal/openebs",
		TargetNS:  "openebs",
		Version:   constant.OpenEBSVersion,
		Values:    values,
		Timeout:   time.Duration(time.Minute * 30), // it takes a while to install openebs
	})
	return helmSpec, nil
}

func yamlifyValues(values map[string]interface{}) (string, error) {
	bytes, err := yaml.Marshal(values)
	if err != nil {
		return "", err
	}
	return string(bytes), nil
}

// reconcileHelmExtensions creates instance of Chart CR for each chart of the config file
// it also reconciles repositories settings
// the actual helm install/update/delete management is done by ChartReconciler structure
func (ec *ExtensionsController) reconcileHelmExtensions(helmSpec *k0sAPI.HelmExtensions) error {
	if helmSpec == nil {
		return nil
	}

	for _, repo := range helmSpec.Repositories {
		if err := ec.addRepo(repo); err != nil {
			return fmt.Errorf("can't init repository %q: %w", repo.URL, err)
		}
	}

	for _, chart := range helmSpec.Charts {
		tw := templatewriter.TemplateWriter{
			Name:     "addon_crd_manifest",
			Template: chartCrdTemplate,
			Data: struct {
				k0sAPI.Chart
				Finalizer string
			}{
				Chart:     chart,
				Finalizer: finalizerName,
			},
		}
		buf := bytes.NewBuffer([]byte{})
		if err := tw.WriteToBuffer(buf); err != nil {
			return fmt.Errorf("can't create chart CR instance %q: %w", chart.ChartName, err)
		}
		if err := ec.saver.Save("addon_crd_manifest_"+chart.Name+".yaml", buf.Bytes()); err != nil {
			return fmt.Errorf("can't save addon CRD manifest for chart CR instance %q: %w", chart.ChartName, err)
		}
	}
	return nil
}

type ChartReconciler struct {
	client.Client
	helm          *helm.Commands
	leaderElector leaderelector.Interface
	L             *logrus.Entry
}

func (cr *ChartReconciler) InjectClient(c client.Client) error {
	cr.Client = c
	return nil
}

func (cr *ChartReconciler) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	if !cr.leaderElector.IsLeader() {
		return reconcile.Result{}, nil
	}
	cr.L.Tracef("Got helm chart reconciliation request: %s", req)
	defer cr.L.Tracef("Finished processing helm chart reconciliation request: %s", req)

	var chartInstance v1beta1.Chart

	if err := cr.Client.Get(ctx, req.NamespacedName, &chartInstance); err != nil {
		if errors.IsNotFound(err) {
			return reconcile.Result{}, nil
		}
		return reconcile.Result{}, err
	}

	if !chartInstance.ObjectMeta.DeletionTimestamp.IsZero() {
		cr.L.Debugf("Uninstall reconciliation request: %s", req)
		// uninstall chart
		if err := cr.uninstall(ctx, chartInstance); err != nil {
			return reconcile.Result{}, fmt.Errorf("can't uninstall chart: %w", err)
		}
		controllerutil.RemoveFinalizer(&chartInstance, finalizerName)
		if err := cr.Client.Update(ctx, &chartInstance); err != nil {
			return reconcile.Result{}, err
		}
		return reconcile.Result{}, nil
	}
	cr.L.Debugf("Install or update reconciliation request: %s", req)
	if err := cr.updateOrInstallChart(ctx, chartInstance); err != nil {
		return reconcile.Result{Requeue: true}, fmt.Errorf("can't update or install chart: %w", err)
	}

	cr.L.Debugf("Installed or updated reconciliation request: %s", req)
	return reconcile.Result{}, nil
}
func (cr *ChartReconciler) uninstall(ctx context.Context, chart v1beta1.Chart) error {
	if err := cr.helm.UninstallRelease(ctx, chart.Status.ReleaseName, chart.Status.Namespace); err != nil {
		return fmt.Errorf("can't uninstall release `%s/%s`: %w", chart.Status.Namespace, chart.Status.ReleaseName, err)
	}
	return nil
}

const defaultTimeout = time.Duration(10 * time.Minute)

func (cr *ChartReconciler) updateOrInstallChart(ctx context.Context, chart v1beta1.Chart) error {
	var err error
	var chartRelease *release.Release
	timeout, err := time.ParseDuration(chart.Spec.Timeout)
	if err != nil {
		cr.L.Tracef("Can't parse `%s` as time.Duration, using default timeout `%s`", chart.Spec.Timeout, defaultTimeout)
		timeout = defaultTimeout
	}
	if timeout == 0 {
		cr.L.Tracef("Using default timeout `%s`, failed to parse `%s`", defaultTimeout, chart.Spec.Timeout)
		timeout = defaultTimeout
	}
	defer func() {
		if err == nil {
			return
		}
		if err := apiretry.RetryOnConflict(apiretry.DefaultRetry, func() error {
			return cr.updateStatus(ctx, chart, chartRelease, err)
		}); err != nil {
			cr.L.WithError(err).Error("Failed to update status for chart release, give up", chart.Name)
		}
	}()
	if chart.Status.ReleaseName == "" {
		// new chartRelease
		cr.L.Tracef("Start update or install %s", chart.Spec.ChartName)
		chartRelease, err = cr.helm.InstallChart(ctx,
			chart.Spec.ChartName,
			chart.Spec.Version,
			chart.Spec.ReleaseName,
			chart.Spec.Namespace,
			chart.Spec.YamlValues(),
			timeout,
		)
		if err != nil {
			return fmt.Errorf("can't reconcile installation for %q: %w", chart.GetName(), err)
		}
	} else {
		if cr.chartNeedsUpgrade(chart) {
			// update
			chartRelease, err = cr.helm.UpgradeChart(ctx,
				chart.Spec.ChartName,
				chart.Spec.Version,
				chart.Status.ReleaseName,
				chart.Status.Namespace,
				chart.Spec.YamlValues(),
				timeout,
			)
			if err != nil {
				return fmt.Errorf("can't reconcile upgrade for %q: %w", chart.GetName(), err)
			}
		}
	}
	if err := apiretry.RetryOnConflict(apiretry.DefaultRetry, func() error {
		return cr.updateStatus(ctx, chart, chartRelease, nil)
	}); err != nil {
		cr.L.WithError(err).Error("Failed to update status for chart release, give up", chart.Name)
	}
	return nil
}

func (cr *ChartReconciler) chartNeedsUpgrade(chart v1beta1.Chart) bool {
	return !(chart.Status.Namespace == chart.Spec.Namespace &&
		chart.Status.ReleaseName == chart.Spec.ReleaseName &&
		chart.Status.Version == chart.Spec.Version &&
		chart.Status.ValuesHash == chart.Spec.HashValues())
}

// updateStatus updates the status of the chart with the given release information. This function
// starts by fetching an updated version of the chart from the api as the install may take a while
// to complete and the chart may have been updated in the meantime. If returns the error returned
// by the Update operation. Moreover, if the chart has indeed changed in the meantime we already
// have an event for it so we will see it again soon.
func (cr *ChartReconciler) updateStatus(ctx context.Context, chart v1beta1.Chart, chartRelease *release.Release, err error) error {
	nsn := types.NamespacedName{Namespace: chart.Namespace, Name: chart.Name}
	var updchart v1beta1.Chart
	if err := cr.Get(ctx, nsn, &updchart); err != nil {
		return fmt.Errorf("can't get updated version of chart %s: %w", chart.Name, err)
	}
	chart.Spec.YamlValues() // XXX what is this function for ?
	if chartRelease != nil {
		updchart.Status.ReleaseName = chartRelease.Name
		updchart.Status.Version = chartRelease.Chart.Metadata.Version
		updchart.Status.AppVersion = chartRelease.Chart.AppVersion()
		updchart.Status.Revision = int64(chartRelease.Version)
		updchart.Status.Namespace = chartRelease.Namespace
	}
	updchart.Status.Updated = time.Now().String()
	updchart.Status.Error = ""
	if err != nil {
		updchart.Status.Error = err.Error()
	}
	updchart.Status.ValuesHash = chart.Spec.HashValues()
	if updErr := cr.Client.Status().Update(ctx, &updchart); updErr != nil {
		cr.L.WithError(updErr).Error("Failed to update status for chart release", chart.Name)
		return updErr
	}
	return nil
}

func (ec *ExtensionsController) addRepo(repo k0sAPI.Repository) error {
	return ec.helm.AddRepository(repo)
}

const chartCrdTemplate = `
apiVersion: helm.k0sproject.io/v1beta1
kind: Chart
metadata:
  name: k0s-addon-chart-{{ .Name }}
  namespace: "kube-system"
  finalizers:
    - {{ .Finalizer }}
spec:
  chartName: {{ .ChartName }}
  releaseName: {{ .Name }}
  timeout: {{ .Timeout }}
  values: |
{{ .Values | nindent 4 }}
  version: {{ .Version }}
  namespace: {{ .TargetNS }}
`

const finalizerName = "helm.k0sproject.io/uninstall-helm-release"

// Init
func (ec *ExtensionsController) Init(_ context.Context) error {
	return nil
}

// Run
func (ec *ExtensionsController) Start(ctx context.Context) error {
	config, err := clientcmd.BuildConfigFromFlags("", ec.kubeConfig)
	if err != nil {
		return fmt.Errorf("can't build controller-runtime controller for helm extensions: %w", err)
	}

	mgr, err := ctrlManager.New(config, ctrlManager.Options{
		MetricsBindAddress: "0",
		Logger:             logrusr.New(ec.L),
	})
	if err != nil {
		return fmt.Errorf("can't build controller-runtime controller for helm extensions: %w", err)
	}
	if err := retry.Do(func() error {
		_, err := mgr.GetRESTMapper().RESTMapping(schema.GroupKind{
			Group: v1beta1.GroupVersion.Group,
			Kind:  "Chart",
		})
		if err != nil {
			ec.L.Warn("Extensions CRD is not yet ready, waiting before starting ExtensionsController")
			return err
		}
		ec.L.Info("Extensions CRD is ready, going nuts")
		return nil
	}, retry.Context(ctx)); err != nil {
		return fmt.Errorf("can't start ExtensionsReconciler, helm CRD is not registred, check CRD registration reconciler: %w", err)
	}
	// examples say to not use GetScheme in production, but it is unclear at the moment
	// which scheme should be in use
	if err := v1beta1.AddToScheme(mgr.GetScheme()); err != nil {
		return fmt.Errorf("can't register Chart crd: %w", err)
	}

	if err := builder.
		ControllerManagedBy(mgr).
		For(&v1beta1.Chart{},
			builder.WithPredicates(predicate.And(
				predicate.GenerationChangedPredicate{},
				predicate.NewPredicateFuncs(func(object client.Object) bool {
					return object.GetNamespace() == namespaceToWatch
				}),
			),
			),
		).
		Complete(&ChartReconciler{
			leaderElector: ec.leaderElector, // TODO: drop in favor of controller-runtime lease manager?
			helm:          ec.helm,
			L:             ec.L.WithField("extensions_type", "helm"),
		}); err != nil {
		return fmt.Errorf("can't build controller-runtime controller for helm extensions: %w", err)
	}

	go func() {
		if err := mgr.Start(ctx); err != nil {
			ec.L.WithError(err).Error("Controller manager working loop exited")
		}
	}()

	return nil
}

// Stop
func (ec *ExtensionsController) Stop() error {
	return nil
}
