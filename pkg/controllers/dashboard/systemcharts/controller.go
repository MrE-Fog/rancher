package systemcharts

import (
	"context"

	catalog "github.com/rancher/rancher/pkg/apis/catalog.cattle.io/v1"
	"github.com/rancher/rancher/pkg/controllers/dashboard/chart"
	"github.com/rancher/rancher/pkg/features"
	namespace "github.com/rancher/rancher/pkg/namespace"
	"github.com/rancher/rancher/pkg/settings"
	"github.com/rancher/rancher/pkg/wrangler"
	corecontrollers "github.com/rancher/wrangler/pkg/generated/controllers/core/v1"
	"github.com/rancher/wrangler/pkg/relatedresource"
	"github.com/sirupsen/logrus"
	"k8s.io/apimachinery/pkg/api/errors"
	apierror "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
)

const (
	repoName         = "rancher-charts"
	webhookChartName = "rancher-webhook"
	webhookImage     = "rancher/rancher-webhook"
)

func Register(ctx context.Context, wContext *wrangler.Context, registryOverride string) error {
	h := &handler{
		manager:          wContext.SystemChartsManager,
		namespaces:       wContext.Core.Namespace(),
		chartsConfig:     chart.RancherConfigGetter{ConfigCache: wContext.Core.ConfigMap().Cache()},
		registryOverride: registryOverride,
	}

	wContext.Catalog.ClusterRepo().OnChange(ctx, "bootstrap-charts", h.onRepo)
	relatedresource.WatchClusterScoped(ctx, "bootstrap-charts", func(namespace, name string, obj runtime.Object) ([]relatedresource.Key, error) {
		return []relatedresource.Key{{
			Name: repoName,
		}}, nil
	}, wContext.Catalog.ClusterRepo(), wContext.Mgmt.Feature())
	return nil
}

type handler struct {
	manager          chart.Manager
	namespaces       corecontrollers.NamespaceController
	chartsConfig     chart.RancherConfigGetter
	registryOverride string
}

func (h *handler) onRepo(key string, repo *catalog.ClusterRepo) (*catalog.ClusterRepo, error) {
	if repo == nil || repo.Name != repoName {
		return repo, nil
	}

	systemGlobalRegistry := map[string]interface{}{
		"cattle": map[string]interface{}{
			"systemDefaultRegistry": settings.SystemDefaultRegistry.Get(),
		},
	}
	if h.registryOverride != "" {
		// if we have a specific image override, don't set the system default registry
		// don't need to check for type assert since we just created this above
		registryMap := systemGlobalRegistry["cattle"].(map[string]interface{})
		registryMap["systemDefaultRegistry"] = ""
		systemGlobalRegistry["cattle"] = registryMap
	}
	for _, chartDef := range h.getChartsToInstall() {
		if chartDef.Enabled != nil && !chartDef.Enabled() {
			continue
		}

		if chartDef.Uninstall {
			if err := h.manager.Uninstall(chartDef.ReleaseNamespace, chartDef.ChartName); err != nil {
				return repo, err
			}
			if chartDef.RemoveNamespace {
				if err := h.namespaces.Delete(chartDef.ReleaseNamespace, nil); err != nil && !errors.IsNotFound(err) {
					return repo, err
				}
			}
			continue
		}

		values := map[string]interface{}{
			"global": systemGlobalRegistry,
		}
		var installImageOverride string
		if h.registryOverride != "" {
			imageSettings, ok := values["image"].(map[string]interface{})
			if !ok {
				imageSettings = map[string]interface{}{}
			}
			imageSettings["repository"] = h.registryOverride + "/" + webhookImage
			values["image"] = imageSettings
			installImageOverride = h.registryOverride + "/" + settings.ShellImage.Get()
		}
		if chartDef.Values != nil {
			for k, v := range chartDef.Values() {
				values[k] = v
			}
		}
		// webhook needs to be able to adopt the MutatingWebhookConfiguration which originally wasn't a part of the
		// chart definition, but is now part of the chart definition
		if err := h.manager.Ensure(chartDef.ReleaseNamespace, chartDef.ChartName, chartDef.MinVersionSetting.Get(), values, chartDef.ChartName == webhookChartName, installImageOverride); err != nil {
			return repo, err
		}
	}

	return repo, nil
}

func (h *handler) getChartsToInstall() []*chart.Definition {
	return []*chart.Definition{
		{
			ReleaseNamespace:  namespace.System,
			ChartName:         webhookChartName,
			MinVersionSetting: settings.RancherWebhookMinVersion,
			Values: func() map[string]interface{} {
				values := map[string]interface{}{
					"capi": map[string]interface{}{
						"enabled": features.EmbeddedClusterAPI.Enabled(),
					},
					"mcm": map[string]interface{}{
						"enabled": features.MCM.Enabled(),
					},
				}
				// add priority class value.
				if priorityClassName, err := h.chartsConfig.GetPriorityClassName(); err != nil {
					if !apierror.IsNotFound(err) {
						logrus.Warnf("Failed to get rancher priorityClassName for 'rancher-webhook': %v", err)
					}
				} else {
					values[chart.PriorityClassKey] = priorityClassName
				}
				return values
			},
			Enabled: func() bool { return true },
		},
		{
			ReleaseNamespace: "rancher-operator-system",
			ChartName:        "rancher-operator",
			Uninstall:        true,
			RemoveNamespace:  true,
		},
	}
}
