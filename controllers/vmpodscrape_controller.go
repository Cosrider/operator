/*


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

package controllers

import (
	"context"

	"github.com/VictoriaMetrics/operator/controllers/factory"
	"github.com/VictoriaMetrics/operator/internal/config"
	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	victoriametricsv1beta1 "github.com/VictoriaMetrics/operator/api/v1beta1"
)

// VMPodScrapeReconciler reconciles a VMPodScrape object
type VMPodScrapeReconciler struct {
	client.Client
	Log          logr.Logger
	OriginScheme *runtime.Scheme
	BaseConf     *config.BaseOperatorConf
}

// Scheme implements interface.
func (r *VMPodScrapeReconciler) Scheme() *runtime.Scheme {
	return r.OriginScheme
}

// Reconcile general reconcile method for controller
// +kubebuilder:rbac:groups=operator.victoriametrics.com,resources=vmpodscrapes,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=operator.victoriametrics.com,resources=vmpodscrapes/status,verbs=get;update;patch
func (r *VMPodScrapeReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	if vmAgentReconcileLimit.MustThrottleReconcile() {
		// fast path, rate limited
		return ctrl.Result{}, nil
	}
	reqLogger := r.Log.WithValues("vmpodscrape", req.NamespacedName)
	reqLogger.Info("Reconciling VMPodScrape")

	// Fetch the VMPodScrape instance
	instance := &victoriametricsv1beta1.VMPodScrape{}
	err := r.Get(ctx, req.NamespacedName, instance)
	if err != nil {
		if !errors.IsNotFound(err) {
			// Error reading the object - requeue the request.
			return ctrl.Result{}, err
		}
	}
	vmAgentSync.Lock()
	defer vmAgentSync.Unlock()

	if !instance.DeletionTimestamp.IsZero() {
		DeregisterObject(instance.Name, instance.Namespace, "vmpodscrape")
	} else {
		RegisterObject(instance.Name, instance.Namespace, "vmpodscrape")
	}

	vmAgentInstances := &victoriametricsv1beta1.VMAgentList{}
	err = r.List(ctx, vmAgentInstances, config.MustGetNamespaceListOptions())
	if err != nil {
		reqLogger.Error(err, "cannot list vmagent objects")
		return ctrl.Result{}, err
	}
	reqLogger.Info("found vmagent objects ", "vmagents count: ", len(vmAgentInstances.Items))

	for _, vmagent := range vmAgentInstances.Items {
		if vmagent.DeletionTimestamp != nil {
			continue
		}
		reqLogger = reqLogger.WithValues("vmagent", vmagent.Name)
		currentVMagent := &vmagent
		match, err := isSelectorsMatches(instance, currentVMagent, currentVMagent.Spec.PodScrapeNamespaceSelector, currentVMagent.Spec.PodScrapeSelector)
		if err != nil {
			reqLogger.Error(err, "cannot match vmagent and vmPodScrape")
			continue
		}
		// fast path
		if !match {
			continue
		}
		reqLogger = reqLogger.WithValues("vmagent", vmagent.Name)
		reqLogger.Info("reconciling podscrape for vmagent")
		recon, err := factory.CreateOrUpdateVMAgent(ctx, currentVMagent, r, r.BaseConf)
		if err != nil {
			reqLogger.Error(err, "cannot create or update vmagent")
			return recon, err
		}
		reqLogger.Info("reconciled vmagent")
	}

	reqLogger.Info("reconciled pod monitor")
	return ctrl.Result{}, nil
}

// SetupWithManager general setup method
func (r *VMPodScrapeReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&victoriametricsv1beta1.VMPodScrape{}).
		WithOptions(defaultOptions).
		Complete(r)
}
