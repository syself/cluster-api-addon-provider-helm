/*
Copyright 2022 The Kubernetes Authors.

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

package helmchartproxy

import (
	"context"
	"fmt"

	addonsv1alpha1 "sigs.k8s.io/cluster-api-addon-provider-helm/api/v1alpha1"

	"sigs.k8s.io/cluster-api-addon-provider-helm/internal"

	"github.com/google/go-cmp/cmp"
	"github.com/pkg/errors"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"
	"sigs.k8s.io/cluster-api/util"
	"sigs.k8s.io/cluster-api/util/conditions"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// deleteOrphanedHelmReleaseProxies deletes any HelmReleaseProxy resources that belong to a Cluster that is not selected by its parent HelmChartProxy.
func (r *HelmChartProxyReconciler) deleteOrphanedHelmReleaseProxies(ctx context.Context, helmChartProxy *addonsv1alpha1.HelmChartProxy, clusters []clusterv1.Cluster, helmReleaseProxies []addonsv1alpha1.HelmReleaseProxy) error {
	log := ctrl.LoggerFrom(ctx)
	helmChartProxyList := &addonsv1alpha1.HelmChartProxyList{}
	if err := r.Client.List(ctx, helmChartProxyList, client.InNamespace(helmChartProxy.Namespace)); err != nil {
		return fmt.Errorf("failed to list HelmChartProxy objects: %w", err)
	}

	hcpForChartList := make([]addonsv1alpha1.HelmChartProxy, 0, len(helmChartProxyList.Items))
	for i, hcp := range helmChartProxyList.Items {
		// list all helmChartProxies that reference the same chart, except for the object that we reconcile already
		if hcp.Name != helmChartProxy.Name && hcp.Spec.ChartName == helmChartProxy.Spec.ChartName {
			hcpForChartList = append(hcpForChartList, helmChartProxyList.Items[i])
		}
	}

	releasesToDelete, releasesToOrphan, err := r.getOrphanedHelmReleaseProxies(ctx, clusters, helmReleaseProxies, hcpForChartList)
	if err != nil {
		return fmt.Errorf("failed to get orphaned HelmReleaseProxies: %w", err)
	}

	log.V(2).Info("Deleting orphaned releases")
	for _, release := range releasesToDelete {
		log.V(2).Info("Deleting release", "release", release)
		if err := r.deleteHelmReleaseProxy(ctx, &release); err != nil {
			conditions.MarkFalse(helmChartProxy, addonsv1alpha1.HelmReleaseProxySpecsUpToDateCondition, addonsv1alpha1.HelmReleaseProxyDeletionFailedReason, clusterv1.ConditionSeverityError, err.Error())
			return err
		}
	}

	log.V(2).Info("Removing labels of orphaned releases")
	for _, release := range releasesToOrphan {
		log.V(2).Info("Removing labels of orphaned release", "release", release)
		if err := r.removeOwnerOfHelmReleaseProxy(ctx, helmChartProxy, &release); err != nil {
			conditions.MarkFalse(helmChartProxy, addonsv1alpha1.HelmReleaseProxySpecsUpToDateCondition, addonsv1alpha1.HelmReleaseProxyDeletionFailedReason, clusterv1.ConditionSeverityError, err.Error())
			return err
		}
	}

	return nil
}

// reconcileForCluster will create or update a HelmReleaseProxy for the given cluster.
func (r *HelmChartProxyReconciler) reconcileForCluster(ctx context.Context, helmChartProxy *addonsv1alpha1.HelmChartProxy, cluster clusterv1.Cluster) error {
	log := ctrl.LoggerFrom(ctx)

	existingHelmReleaseProxy, err := r.getExistingHelmReleaseProxy(ctx, helmChartProxy, &cluster)
	if err != nil {
		// TODO: Should we set a condition here?
		return errors.Wrapf(err, "failed to get HelmReleaseProxy for cluster %s", cluster.Name)
	}
	// log.V(2).Info("Found existing HelmReleaseProxy", "cluster", cluster.Name, "release", existingHelmReleaseProxy.Name)

	if existingHelmReleaseProxy != nil && shouldReinstallHelmRelease(ctx, existingHelmReleaseProxy, helmChartProxy) {
		log.V(2).Info("Reinstalling Helm release by deleting and creating HelmReleaseProxy", "helmReleaseProxy", existingHelmReleaseProxy.Name)
		if err := r.deleteHelmReleaseProxy(ctx, existingHelmReleaseProxy); err != nil {
			conditions.MarkFalse(helmChartProxy, addonsv1alpha1.HelmReleaseProxySpecsUpToDateCondition, addonsv1alpha1.HelmReleaseProxyDeletionFailedReason, clusterv1.ConditionSeverityError, err.Error())

			return err
		}

		// TODO: Add a check on requeue to make sure that the HelmReleaseProxy isn't still deleting
		log.V(2).Info("Successfully deleted HelmReleaseProxy on cluster, returning to requeue for reconcile", "cluster", cluster.Name)
		conditions.MarkFalse(helmChartProxy, addonsv1alpha1.HelmReleaseProxySpecsUpToDateCondition, addonsv1alpha1.HelmReleaseProxyReinstallingReason, clusterv1.ConditionSeverityInfo, "HelmReleaseProxy on cluster '%s' successfully deleted, preparing to reinstall", cluster.Name)
		return nil // Try returning early so it will requeue
		// TODO: should we continue in the loop or just requeue?
	}

	existingHelmReleaseProxy, err = r.getOrphanedHelmReleaseProxy(ctx, helmChartProxy, &cluster)
	if err != nil {
		// TODO: Should we set a condition here?
		return errors.Wrapf(err, "failed to get orphaned HelmReleaseProxy for cluster %s", cluster.Name)
	}

	values, err := internal.ParseValues(ctx, r.Client, helmChartProxy.Spec, &cluster)
	if err != nil {
		conditions.MarkFalse(helmChartProxy, addonsv1alpha1.HelmReleaseProxySpecsUpToDateCondition, addonsv1alpha1.ValueParsingFailedReason, clusterv1.ConditionSeverityError, err.Error())

		return errors.Wrapf(err, "failed to parse values on cluster %s", cluster.Name)
	}

	log.V(2).Info("Values for cluster", "cluster", cluster.Name, "values", values)
	if err := r.createOrUpdateHelmReleaseProxy(ctx, existingHelmReleaseProxy, helmChartProxy, &cluster, values); err != nil {
		conditions.MarkFalse(helmChartProxy, addonsv1alpha1.HelmReleaseProxySpecsUpToDateCondition, addonsv1alpha1.HelmReleaseProxyCreationFailedReason, clusterv1.ConditionSeverityError, err.Error())

		return errors.Wrapf(err, "failed to create or update HelmReleaseProxy on cluster %s", cluster.Name)
	}
	return nil
}

// getExistingHelmReleaseProxy returns the HelmReleaseProxy for the given cluster if it exists.
func (r *HelmChartProxyReconciler) getExistingHelmReleaseProxy(ctx context.Context, helmChartProxy *addonsv1alpha1.HelmChartProxy, cluster *clusterv1.Cluster) (*addonsv1alpha1.HelmReleaseProxy, error) {
	log := ctrl.LoggerFrom(ctx)

	helmReleaseProxyList := &addonsv1alpha1.HelmReleaseProxyList{}

	listOpts := []client.ListOption{
		client.MatchingLabels{
			clusterv1.ClusterNameLabel:             cluster.Name,
			addonsv1alpha1.HelmChartProxyLabelName: helmChartProxy.Name,
		},
	}

	// TODO: Figure out if we want this search to be cross-namespaces.

	log.V(2).Info("Attempting to fetch existing HelmReleaseProxy with Cluster and HelmChartProxy labels", "cluster", cluster.Name, "helmChartProxy", helmChartProxy.Name)
	if err := r.Client.List(context.TODO(), helmReleaseProxyList, listOpts...); err != nil {
		return nil, err
	}

	if helmReleaseProxyList.Items == nil || len(helmReleaseProxyList.Items) == 0 {
		log.V(2).Info("No HelmReleaseProxy found matching the cluster and HelmChartProxy", "cluster", cluster.Name, "helmChartProxy", helmChartProxy.Name)
		return nil, nil
	} else if len(helmReleaseProxyList.Items) > 1 {
		log.V(2).Info("Multiple HelmReleaseProxies found matching the cluster and HelmChartProxy", "cluster", cluster.Name, "helmChartProxy", helmChartProxy.Name)
		return nil, errors.Errorf("multiple HelmReleaseProxies found matching the cluster and HelmChartProxy")
	}

	log.V(2).Info("Found existing matching HelmReleaseProxy", "cluster", cluster.Name, "helmChartProxy", helmChartProxy.Name)

	return &helmReleaseProxyList.Items[0], nil
}

// getOrphanedHelmReleaseProxy returns an orphaned HelmReleaseProxy for the given cluster and helm chart if it exists.
func (r *HelmChartProxyReconciler) getOrphanedHelmReleaseProxy(ctx context.Context, helmChartProxy *addonsv1alpha1.HelmChartProxy, cluster *clusterv1.Cluster) (*addonsv1alpha1.HelmReleaseProxy, error) {
	log := ctrl.LoggerFrom(ctx)

	helmReleaseProxyList := &addonsv1alpha1.HelmReleaseProxyList{}

	listOpts := []client.ListOption{
		client.MatchingLabels{
			clusterv1.ClusterNameLabel: cluster.Name,
		},
	}

	// TODO: Figure out if we want this search to be cross-namespaces.

	log.V(2).Info("Attempting to fetch existing HelmReleaseProxy with Cluster and HelmChartProxy labels", "cluster", cluster.Name, "helmChartProxy", helmChartProxy.Name)
	if err := r.Client.List(ctx, helmReleaseProxyList, listOpts...); err != nil {
		return nil, err
	}

	if helmReleaseProxyList.Items == nil || len(helmReleaseProxyList.Items) == 0 {
		log.V(2).Info("No HelmReleaseProxy found matching the cluster and HelmChartProxy", "cluster", cluster.Name, "helmChartProxy", helmChartProxy.Name)
		return nil, nil
	}

	orphanedHelmReleaseProxiesWithSameChart := make([]*addonsv1alpha1.HelmReleaseProxy, 0, len(helmReleaseProxyList.Items))

	for i, helmReleaseProxy := range helmReleaseProxyList.Items {
		if helmReleaseProxy.Spec.ChartName == helmChartProxy.Spec.ChartName {
			// only take helmReleaseProxies which are orphaned, i.e. have no helmChartProxyLabel
			if _, ok := helmReleaseProxy.Labels[addonsv1alpha1.HelmChartProxyLabelName]; !ok {
				orphanedHelmReleaseProxiesWithSameChart = append(orphanedHelmReleaseProxiesWithSameChart, &helmReleaseProxyList.Items[i])
			}
		}
	}

	if len(orphanedHelmReleaseProxiesWithSameChart) == 0 {
		log.V(2).Info("No orphaned HelmReleaseProxy found matching the cluster and HelmChartProxy", "cluster", cluster.Name, "helmChartProxy", helmChartProxy.Name)
		return nil, nil
	}

	if len(orphanedHelmReleaseProxiesWithSameChart) > 1 {
		log.V(2).Info("Multiple orphaned HelmReleaseProxies found matching the cluster and HelmChartProxy", "cluster", cluster.Name, "helmChartProxy", helmChartProxy.Name)
		return nil, errors.Errorf("multiple orphaned HelmReleaseProxies found matching the cluster and HelmChartProxy")
	}

	log.V(2).Info("Found existing matching orphaned HelmReleaseProxy", "cluster", cluster.Name, "helmChartProxy", helmChartProxy.Name)

	return orphanedHelmReleaseProxiesWithSameChart[0], nil
}

// createOrUpdateHelmReleaseProxy creates or updates the HelmReleaseProxy for the given cluster.
func (r *HelmChartProxyReconciler) createOrUpdateHelmReleaseProxy(ctx context.Context, existing *addonsv1alpha1.HelmReleaseProxy, helmChartProxy *addonsv1alpha1.HelmChartProxy, cluster *clusterv1.Cluster, parsedValues string) error {
	log := ctrl.LoggerFrom(ctx)
	helmReleaseProxy := constructHelmReleaseProxy(existing, helmChartProxy, parsedValues, cluster)
	if helmReleaseProxy == nil {
		log.V(2).Info("HelmReleaseProxy is up to date, nothing to do", "helmReleaseProxy", existing.Name, "cluster", cluster.Name)
		return nil
	}
	if existing == nil {
		if err := r.Client.Create(ctx, helmReleaseProxy); err != nil {
			return errors.Wrapf(err, "failed to create HelmReleaseProxy '%s' for cluster: %s/%s", helmReleaseProxy.Name, cluster.Namespace, cluster.Name)
		}
	} else {
		// TODO: should this use patchHelmReleaseProxy() instead of Update() in case there's a race condition?
		if err := r.Client.Update(ctx, helmReleaseProxy); err != nil {
			return errors.Wrapf(err, "failed to update HelmReleaseProxy '%s' for cluster: %s/%s", helmReleaseProxy.Name, cluster.Namespace, cluster.Name)
		}
	}

	return nil
}

// deleteHelmReleaseProxy deletes the HelmReleaseProxy for the given cluster.
func (r *HelmChartProxyReconciler) deleteHelmReleaseProxy(ctx context.Context, helmReleaseProxy *addonsv1alpha1.HelmReleaseProxy) error {
	log := ctrl.LoggerFrom(ctx)

	if err := r.Client.Delete(ctx, helmReleaseProxy); err != nil {
		if apierrors.IsNotFound(err) {
			log.V(2).Info("HelmReleaseProxy already deleted, nothing to do", "helmReleaseProxy", helmReleaseProxy.Name)
			return nil
		}
		return errors.Wrapf(err, "failed to delete helmReleaseProxy: %s", helmReleaseProxy.Name)
	}

	return nil
}

// removeOwnerOfHelmReleaseProxy removes owner of the HelmReleaseProxy for the given cluster.
func (r *HelmChartProxyReconciler) removeOwnerOfHelmReleaseProxy(ctx context.Context, helmChartProxy *addonsv1alpha1.HelmChartProxy, helmReleaseProxy *addonsv1alpha1.HelmReleaseProxy) error {
	log := ctrl.LoggerFrom(ctx)

	// remove label to helmChartProxy
	delete(helmReleaseProxy.Labels, addonsv1alpha1.HelmChartProxyLabelName)

	// remove owner references of helmChartProxy
	name := helmChartProxy.Name
	kind := helmChartProxy.Kind
	apiVersion := helmChartProxy.APIVersion
	helmReleaseProxy.OwnerReferences = removeOwnerRefFromList(helmReleaseProxy.OwnerReferences, name, kind, apiVersion)

	if err := r.Client.Update(ctx, helmReleaseProxy); err != nil {
		if apierrors.IsNotFound(err) {
			log.V(2).Info("HelmReleaseProxy deleted, cannot update", "helmReleaseProxy", helmReleaseProxy.Name)
			return nil
		}
		return errors.Wrapf(err, "failed to remove owner of helmReleaseProxy: %s", helmReleaseProxy.Name)
	}

	return nil
}

// constructHelmReleaseProxy constructs a new HelmReleaseProxy for the given Cluster or updates the existing HelmReleaseProxy if needed.
// If no update is needed, this returns nil. Note that this does not check if we need to reinstall the HelmReleaseProxy, i.e. immutable fields changed.
func constructHelmReleaseProxy(existing *addonsv1alpha1.HelmReleaseProxy, helmChartProxy *addonsv1alpha1.HelmChartProxy, parsedValues string, cluster *clusterv1.Cluster) *addonsv1alpha1.HelmReleaseProxy {
	helmReleaseProxy := &addonsv1alpha1.HelmReleaseProxy{}
	if existing == nil {
		helmReleaseProxy.GenerateName = fmt.Sprintf("%s-%s-", helmChartProxy.Spec.ChartName, cluster.Name)
		helmReleaseProxy.Namespace = helmChartProxy.Namespace
		helmReleaseProxy.OwnerReferences = util.EnsureOwnerRef(helmReleaseProxy.OwnerReferences, *metav1.NewControllerRef(helmChartProxy, helmChartProxy.GroupVersionKind()))

		newLabels := map[string]string{}
		newLabels[clusterv1.ClusterNameLabel] = cluster.Name
		newLabels[addonsv1alpha1.HelmChartProxyLabelName] = helmChartProxy.Name
		helmReleaseProxy.Labels = newLabels

		helmReleaseProxy.Spec.ClusterRef = corev1.ObjectReference{
			Kind:       cluster.Kind,
			APIVersion: cluster.APIVersion,
			Name:       cluster.Name,
			Namespace:  cluster.Namespace,
		}

		helmReleaseProxy.Spec.ReleaseName = helmChartProxy.Spec.ReleaseName
		helmReleaseProxy.Spec.ChartName = helmChartProxy.Spec.ChartName
		helmReleaseProxy.Spec.RepoURL = helmChartProxy.Spec.RepoURL
		helmReleaseProxy.Spec.ReleaseNamespace = helmChartProxy.Spec.ReleaseNamespace

		// helmChartProxy.ObjectMeta.SetAnnotations(helmReleaseProxy.Annotations)
	} else {
		helmReleaseProxy = existing
		changed := false

		// Check if helmReleaseProxy is owned by helmChartProxy. If not, add the owner reference.
		if !util.IsOwnedByObject(helmChartProxy, helmReleaseProxy) {
			helmReleaseProxy.OwnerReferences = util.EnsureOwnerRef(helmReleaseProxy.OwnerReferences, *metav1.NewControllerRef(helmChartProxy, helmChartProxy.GroupVersionKind()))
		}

		// set labels in case the existing helmReleaseProxy has been orphaned
		// labels should never be nil, but it is added just in case. There should always be the cluster name on the orphaned resource.
		if helmReleaseProxy.Labels == nil {
			helmReleaseProxy.Labels = map[string]string{}
			helmReleaseProxy.Labels[clusterv1.ClusterNameLabel] = cluster.Name
			changed = true
		}

		if _, ok := helmReleaseProxy.Labels[addonsv1alpha1.HelmChartProxyLabelName]; !ok {
			helmReleaseProxy.Labels[addonsv1alpha1.HelmChartProxyLabelName] = helmChartProxy.Name
			changed = true
		}

		if existing.Spec.Version != helmChartProxy.Spec.Version {
			changed = true
		}
		if !cmp.Equal(existing.Spec.Values, parsedValues) {
			changed = true
		}

		if !changed {
			return nil
		}
	}

	helmReleaseProxy.Spec.Version = helmChartProxy.Spec.Version
	helmReleaseProxy.Spec.Values = parsedValues

	return helmReleaseProxy
}

// shouldReinstallHelmRelease returns true if the HelmReleaseProxy needs to be reinstalled. This is the case if any of the immutable fields changed.
func shouldReinstallHelmRelease(ctx context.Context, existing *addonsv1alpha1.HelmReleaseProxy, helmChartProxy *addonsv1alpha1.HelmChartProxy) bool {
	log := ctrl.LoggerFrom(ctx)

	log.V(2).Info("Checking if HelmReleaseProxy needs to be reinstalled by by checking if immutable fields changed", "helmReleaseProxy", existing.Name)

	annotations := existing.GetAnnotations()
	result, ok := annotations[addonsv1alpha1.IsReleaseNameGeneratedAnnotation]

	isReleaseNameGenerated := ok && result == "true"
	switch {
	case existing.Spec.ChartName != helmChartProxy.Spec.ChartName:
		log.V(2).Info("ChartName changed", "existing", existing.Spec.ChartName, "helmChartProxy", helmChartProxy.Spec.ChartName)
	case existing.Spec.RepoURL != helmChartProxy.Spec.RepoURL:
		log.V(2).Info("RepoURL changed", "existing", existing.Spec.RepoURL, "helmChartProxy", helmChartProxy.Spec.RepoURL)
	case isReleaseNameGenerated && helmChartProxy.Spec.ReleaseName != "":
		log.V(2).Info("Generated ReleaseName changed", "existing", existing.Spec.ReleaseName, "helmChartProxy", helmChartProxy.Spec.ReleaseName)
	case !isReleaseNameGenerated && existing.Spec.ReleaseName != helmChartProxy.Spec.ReleaseName:
		log.V(2).Info("Non-generated ReleaseName changed", "existing", existing.Spec.ReleaseName, "helmChartProxy", helmChartProxy.Spec.ReleaseName)
	case existing.Spec.ReleaseNamespace != helmChartProxy.Spec.ReleaseNamespace:
		log.V(2).Info("ReleaseNamespace changed", "existing", existing.Spec.ReleaseNamespace, "helmChartProxy", helmChartProxy.Spec.ReleaseNamespace)
		return true
	}

	return false
}

// getOrphanedHelmReleaseProxies returns a list of HelmReleaseProxies that are not associated with any of the selected Clusters for a given HelmChartProxy.
func (r *HelmChartProxyReconciler) getOrphanedHelmReleaseProxies(
	ctx context.Context,
	clusters []clusterv1.Cluster,
	helmReleaseProxies []addonsv1alpha1.HelmReleaseProxy,
	otherhelmChartProxiesWithSameChart []addonsv1alpha1.HelmChartProxy,
) (releasesToDelete []addonsv1alpha1.HelmReleaseProxy, releasesToOrphan []addonsv1alpha1.HelmReleaseProxy, err error) {
	log := ctrl.LoggerFrom(ctx)
	log.V(2).Info("Getting HelmReleaseProxies to delete")

	selectedClusters := map[string]struct{}{}
	for _, cluster := range clusters {
		key := cluster.GetNamespace() + "/" + cluster.GetName()
		selectedClusters[key] = struct{}{}
	}
	log.V(2).Info("Selected clusters", "clusters", selectedClusters)

	for _, helmReleaseProxy := range helmReleaseProxies {
		clusterRef := helmReleaseProxy.Spec.ClusterRef
		key := clusterRef.Namespace + "/" + clusterRef.Name
		if _, ok := selectedClusters[key]; !ok {
			// The cluster does not match the label selector of the helmChartProxy anymore.
			// Instead of deleting the respective helmReleaseObject immediately, we check whether
			// the cluster matches the label selector of another helmChartProxy which has the same chart.
			// If that is the case, we don't want to delete the helmReleaseProxy, as this would remove the chart of the cluster
			// and install it again afterwards. Instead, we want to only trigger a delete if the cluster should not have the
			// respective Helmchart installed at all.

			// get cluster object to check whether label matches the selector of another relevant helmChartProxy now
			var cluster clusterv1.Cluster
			namespacedName := types.NamespacedName{Namespace: clusterRef.Namespace, Name: clusterRef.Name}

			if err := r.Client.Get(ctx, namespacedName, &cluster); err != nil && !apierrors.IsNotFound(err) {
				// if cluster has been deleted, it shouldn't block this operation
				return nil, nil, fmt.Errorf("failed to get cluster: %w", err)
			}

			var toOrphan bool
			for _, helmChartProxy := range otherhelmChartProxiesWithSameChart {
				labelselector, err := metav1.LabelSelectorAsSelector(&helmChartProxy.Spec.ClusterSelector)
				if err != nil {
					return nil, nil, fmt.Errorf("failed to create label selector: %w", err)
				}

				if labelselector.Matches(labels.Set(cluster.Labels)) {
					releasesToOrphan = append(releasesToOrphan, helmReleaseProxy)
					toOrphan = true
					break
				}
			}

			if !toOrphan {
				releasesToDelete = append(releasesToDelete, helmReleaseProxy)
			}
		}
	}

	{
		names := make([]string, len(releasesToDelete))
		for _, release := range releasesToDelete {
			names = append(names, release.Name)
		}
		log.V(2).Info("Releases to delete", "releases", names)
	}
	{
		names := make([]string, len(releasesToOrphan))
		for _, release := range releasesToOrphan {
			names = append(names, release.Name)
		}
		log.V(2).Info("Releases to orphan", "releases", names)
	}
	return releasesToDelete, releasesToOrphan, nil
}

// removeOwnerRefFromList removes the owner reference of a Kubernetes object.
func removeOwnerRefFromList(refList []metav1.OwnerReference, name, kind, apiVersion string) []metav1.OwnerReference {
	if len(refList) == 0 {
		return refList
	}
	index, found := findOwnerRefFromList(refList, name, kind, apiVersion)
	// if owner ref is not found, return
	if !found {
		return refList
	}

	// if it is the only owner ref, we can return an empty slice
	if len(refList) == 1 {
		return []metav1.OwnerReference{}
	}

	// remove owner ref from slice
	refListLen := len(refList) - 1
	refList[index] = refList[refListLen]
	refList = refList[:refListLen]

	return removeOwnerRefFromList(refList, name, kind, apiVersion)
}

// findOwnerRefFromList finds the owner ref of a Kubernetes object in a list of owner refs.
func findOwnerRefFromList(refList []metav1.OwnerReference, name, kind, apiVersion string) (ref int, found bool) {
	bGV, err := schema.ParseGroupVersion(apiVersion)
	if err != nil {
		panic("object has invalid group version")
	}

	for i, curOwnerRef := range refList {
		aGV, err := schema.ParseGroupVersion(curOwnerRef.APIVersion)
		if err != nil {
			// ignore owner ref if it has invalid group version
			continue
		}

		// not matching on UID since when pivoting it might change
		// Not matching on API version as this might change
		if curOwnerRef.Name == name &&
			curOwnerRef.Kind == kind &&
			aGV.Group == bGV.Group {
			return i, true
		}
	}
	return 0, false
}
