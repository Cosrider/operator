package factory

import (
	"context"
	"fmt"
	"k8s.io/apimachinery/pkg/runtime"
	"path"
	"sort"
	"strconv"
	"strings"

	victoriametricsv1beta1 "github.com/VictoriaMetrics/operator/api/v1beta1"
	"github.com/VictoriaMetrics/operator/controllers/factory/finalize"
	"github.com/VictoriaMetrics/operator/controllers/factory/k8stools"
	"github.com/VictoriaMetrics/operator/controllers/factory/psp"
	"github.com/VictoriaMetrics/operator/controllers/factory/vmagent"
	"github.com/VictoriaMetrics/operator/internal/config"
	"github.com/hashicorp/go-version"
	"gopkg.in/yaml.v2"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

const (
	vmAgentConfDir                  = "/etc/vmagent/config"
	vmAgentConOfOutDir              = "/etc/vmagent/config_out"
	vmAgentPersistentQueueDir       = "/tmp/vmagent-remotewrite-data"
	vmAgentPersistentQueueSTSDir    = "/vmagent_pq/vmagent-remotewrite-data"
	vmAgentPersistentQueueMountName = "persistent-queue-data"
	globalRelabelingName            = "global_relabeling.yaml"
	urlRelabelingName               = "url_rebaling-%d.yaml"
)

func CreateOrUpdateVMAgentService(ctx context.Context, cr *victoriametricsv1beta1.VMAgent, rclient client.Client, c *config.BaseOperatorConf) (*corev1.Service, error) {
	cr = cr.DeepCopy()
	if cr.Spec.Port == "" {
		cr.Spec.Port = c.VMAgentDefault.Port
	}
	additionalService := buildDefaultService(cr, cr.Spec.Port, nil)
	mergeServiceSpec(additionalService, cr.Spec.ServiceSpec)
	buildAdditionalServicePorts(cr.Spec.InsertPorts, additionalService)

	newService := buildDefaultService(cr, cr.Spec.Port, nil)
	buildAdditionalServicePorts(cr.Spec.InsertPorts, newService)

	if cr.Spec.ServiceSpec != nil {
		if additionalService.Name == newService.Name {
			log.Error(fmt.Errorf("vmagent additional service name: %q cannot be the same as crd.prefixedname: %q", additionalService.Name, newService.Name), "cannot create additional service")
		} else if _, err := reconcileServiceForCRD(ctx, rclient, additionalService); err != nil {
			return nil, err
		}
	}

	rca := finalize.RemoveSvcArgs{SelectorLabels: cr.SelectorLabels, GetNameSpace: cr.GetNamespace, PrefixedName: cr.PrefixedName}
	if err := finalize.RemoveOrphanedServices(ctx, rclient, rca, cr.Spec.ServiceSpec); err != nil {
		return nil, err
	}

	return reconcileServiceForCRD(ctx, rclient, newService)
}

func CreateOrUpdateVMAgent(ctx context.Context, cr *victoriametricsv1beta1.VMAgent, rclient client.Client, c *config.BaseOperatorConf) (reconcile.Result, error) {
	l := log.WithValues("controller", "vmagent.crud", "namespace", cr.Namespace, "vmagent", cr.PrefixedName())
	if err := psp.CreateServiceAccountForCRD(ctx, cr, rclient); err != nil {
		return reconcile.Result{}, fmt.Errorf("failed create service account: %w", err)
	}
	if c.PSPAutoCreateEnabled {
		if err := psp.CreateOrUpdateServiceAccountWithPSP(ctx, cr, rclient); err != nil {
			l.Error(err, "cannot create podsecuritypolicy")
			return reconcile.Result{}, fmt.Errorf("cannot create podsecurity policy for vmagent, err: %w", err)
		}
	}
	if cr.GetServiceAccountName() == cr.PrefixedName() {
		l.Info("creating default clusterrole for vmagent")
		if err := vmagent.CreateVMAgentClusterAccess(ctx, cr, rclient); err != nil {
			return reconcile.Result{}, fmt.Errorf("cannot create vmagent clusterole and binding for it, err: %w", err)
		}
	}
	// we have to create empty or full cm first
	err := CreateOrUpdateConfigurationSecret(ctx, cr, rclient, c)
	if err != nil {
		l.Error(err, "cannot create configmap")
		return reconcile.Result{}, err
	}

	err = CreateOrUpdateTlsAssets(ctx, cr, rclient)
	if err != nil {
		return reconcile.Result{}, fmt.Errorf("cannot update tls asset for vmagent: %w", err)
	}

	if err := CreateOrUpdateRelabelConfigsAssets(ctx, cr, rclient); err != nil {
		return reconcile.Result{}, fmt.Errorf("cannot update relabeling asset for vmagent: %w", err)
	}

	if cr.Spec.PodDisruptionBudget != nil {
		err = CreateOrUpdatePodDisruptionBudget(ctx, rclient, cr, cr.Kind, cr.Spec.PodDisruptionBudget)
		if err != nil {
			return reconcile.Result{}, fmt.Errorf("cannot update pod disruption budget for vmagent: %w", err)
		}
	}

	// getting secrets for remotewrite spec
	ssCache, err := LoadRemoteWriteSecrets(ctx, cr, rclient)
	if err != nil {
		return reconcile.Result{}, fmt.Errorf("cannot get remote write secrets for vmagent: %w", err)
	}
	l.Info("create or update vm agent deploy")

	newDeploy, err := newDeployForVMAgent(cr, c, ssCache)
	if err != nil {
		return reconcile.Result{}, fmt.Errorf("cannot build new deploy for vmagent: %w", err)
	}

	deploymentNames := make(map[string]struct{})
	stsNames := make(map[string]struct{})
	if cr.Spec.ShardCount != nil && *cr.Spec.ShardCount > 1 {
		shardsCount := *cr.Spec.ShardCount
		l.Info("using cluster version of VMAgent with", "shards", shardsCount)
		for i := 0; i < shardsCount; i++ {
			shardedDeploy := newDeploy.DeepCopyObject()
			addShardSettingsToVMAgent(i, shardsCount, shardedDeploy)
			switch shardedDeploy := shardedDeploy.(type) {
			case *appsv1.Deployment:
				if err := k8stools.HandleDeployUpdate(ctx, rclient, shardedDeploy); err != nil {
					return reconcile.Result{}, err
				}
				deploymentNames[shardedDeploy.Name] = struct{}{}
			case *appsv1.StatefulSet:
				stsOpts := k8stools.STSOptions{
					HasClaim: len(shardedDeploy.Spec.VolumeClaimTemplates) > 0,
					SelectorLabels: func() map[string]string {
						selectorLabels := cr.SelectorLabels()
						selectorLabels["shard-num"] = strconv.Itoa(i)
						return selectorLabels
					},
					VolumeName: func() string {
						return vmAgentPersistentQueueMountName
					},
					UpdateStrategy: cr.STSUpdateStrategy,
				}
				if err := k8stools.HandleSTSUpdate(ctx, rclient, stsOpts, shardedDeploy, c); err != nil {
					return reconcile.Result{}, err
				}
				stsNames[shardedDeploy.Name] = struct{}{}
			}
		}
	} else {
		switch newDeploy := newDeploy.(type) {
		case *appsv1.Deployment:
			if err := k8stools.HandleDeployUpdate(ctx, rclient, newDeploy); err != nil {
				return reconcile.Result{}, err
			}
			deploymentNames[newDeploy.Name] = struct{}{}
		case *appsv1.StatefulSet:
			stsOpts := k8stools.STSOptions{
				HasClaim:       len(newDeploy.Spec.VolumeClaimTemplates) > 0,
				SelectorLabels: cr.SelectorLabels,
				VolumeName: func() string {
					return vmAgentPersistentQueueMountName
				},
				UpdateStrategy: cr.STSUpdateStrategy,
			}
			if err := k8stools.HandleSTSUpdate(ctx, rclient, stsOpts, newDeploy, c); err != nil {
				return reconcile.Result{}, err
			}
			stsNames[newDeploy.Name] = struct{}{}
		}

	}
	if err := finalize.RemoveOrphanedDeployments(ctx, rclient, cr, deploymentNames); err != nil {
		return reconcile.Result{}, err
	}
	if err := finalize.RemoveOrphanedSTSs(ctx, rclient, cr, stsNames); err != nil {
		return reconcile.Result{}, err
	}
	l.Info("vmagent reconciled")

	return reconcile.Result{}, nil
}

func setDefaultForVMAgent(cr *victoriametricsv1beta1.VMAgent, c *config.BaseOperatorConf) {
	// inject default
	if cr.Spec.Image.Repository == "" {
		cr.Spec.Image.Repository = c.VMAgentDefault.Image
	}
	if cr.Spec.Image.Tag == "" {
		cr.Spec.Image.Tag = c.VMAgentDefault.Version
	}
	if cr.Spec.Image.PullPolicy == "" {
		cr.Spec.Image.PullPolicy = corev1.PullIfNotPresent
	}

	if cr.Spec.Port == "" {
		cr.Spec.Port = c.VMAgentDefault.Port
	}
}

// newDeployForVMAgent builds vmagent deployment spec.
func newDeployForVMAgent(cr *victoriametricsv1beta1.VMAgent, c *config.BaseOperatorConf, ssCache *scrapesSecretsCache) (runtime.Object, error) {
	cr = cr.DeepCopy()
	setDefaultForVMAgent(cr, c)

	podSpec, err := makeSpecForVMAgent(cr, c, ssCache)
	if err != nil {
		return nil, err
	}

	// fast path, use sts
	if cr.Spec.StatefulMode {
		stsSpec := &appsv1.StatefulSet{
			ObjectMeta: metav1.ObjectMeta{
				Name:            cr.PrefixedName(),
				Namespace:       cr.Namespace,
				Labels:          c.Labels.Merge(cr.AllLabels()),
				Annotations:     cr.AnnotationsFiltered(),
				OwnerReferences: cr.AsOwner(),
				Finalizers:      []string{victoriametricsv1beta1.FinalizerName},
			},
			Spec: appsv1.StatefulSetSpec{
				Replicas: cr.Spec.ReplicaCount,
				Selector: &metav1.LabelSelector{
					MatchLabels: cr.SelectorLabels(),
				},
				UpdateStrategy: appsv1.StatefulSetUpdateStrategy{
					Type: appsv1.OnDeleteStatefulSetStrategyType,
				},
				Template: corev1.PodTemplateSpec{
					ObjectMeta: metav1.ObjectMeta{
						Labels:      cr.PodLabels(),
						Annotations: cr.PodAnnotations(),
					},
					Spec: *podSpec,
				},
			},
		}
		cr.Spec.StatefulStorage.IntoSTSVolume(vmAgentPersistentQueueMountName, &stsSpec.Spec)
		cr.Spec.ClaimTemplates = append(cr.Spec.ClaimTemplates, cr.Spec.ClaimTemplates...)
		return stsSpec, nil
	}

	strategyType := appsv1.RollingUpdateDeploymentStrategyType
	if cr.Spec.UpdateStrategy != nil {
		strategyType = *cr.Spec.UpdateStrategy
	}
	depSpec := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:            cr.PrefixedName(),
			Namespace:       cr.Namespace,
			Labels:          c.Labels.Merge(cr.AllLabels()),
			Annotations:     cr.AnnotationsFiltered(),
			OwnerReferences: cr.AsOwner(),
			Finalizers:      []string{victoriametricsv1beta1.FinalizerName},
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: cr.Spec.ReplicaCount,
			Selector: &metav1.LabelSelector{
				MatchLabels: cr.SelectorLabels(),
			},
			Strategy: appsv1.DeploymentStrategy{
				Type:          strategyType,
				RollingUpdate: cr.Spec.RollingUpdate,
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels:      cr.PodLabels(),
					Annotations: cr.PodAnnotations(),
				},
				Spec: *podSpec,
			},
		},
	}
	return depSpec, nil
}

func makeSpecForVMAgent(cr *victoriametricsv1beta1.VMAgent, c *config.BaseOperatorConf, ssCache *scrapesSecretsCache) (*corev1.PodSpec, error) {
	pqMountPath := vmAgentPersistentQueueDir
	if cr.Spec.StatefulMode {
		pqMountPath = vmAgentPersistentQueueSTSDir
	}
	args := []string{
		fmt.Sprintf("-promscrape.config=%s", path.Join(vmAgentConOfOutDir, configEnvsubstFilename)),
		fmt.Sprintf("-remoteWrite.tmpDataPath=%s", pqMountPath),
	}

	if len(cr.Spec.RemoteWrite) > 0 {
		args = append(args, BuildRemoteWrites(cr, ssCache)...)
	}
	args = append(args, BuildRemoteWriteSettings(cr)...)

	args = append(args, fmt.Sprintf("-httpListenAddr=:%s", cr.Spec.Port))

	if cr.Spec.LogLevel != "" {
		args = append(args, fmt.Sprintf("-loggerLevel=%s", cr.Spec.LogLevel))
	}
	if cr.Spec.LogFormat != "" {
		args = append(args, fmt.Sprintf("-loggerFormat=%s", cr.Spec.LogFormat))
	}
	if len(cr.Spec.ExtraEnvs) > 0 {
		args = append(args, "-envflag.enable=true")
	}

	var envs []corev1.EnvVar

	envs = append(envs, cr.Spec.ExtraEnvs...)

	var ports []corev1.ContainerPort
	ports = append(ports, corev1.ContainerPort{Name: "http", Protocol: "TCP", ContainerPort: intstr.Parse(cr.Spec.Port).IntVal})
	ports = buildAdditionalContainerPorts(ports, cr.Spec.InsertPorts)

	var volumes []corev1.Volume
	// in case for sts, we have to use persistentVolumeClaimTemplate instead
	if !cr.Spec.StatefulMode {
		volumes = append(volumes, corev1.Volume{
			Name: vmAgentPersistentQueueMountName,
			VolumeSource: corev1.VolumeSource{
				EmptyDir: &corev1.EmptyDirVolumeSource{},
			},
		})
	}

	volumes = append(volumes, cr.Spec.Volumes...)
	volumes = append(volumes, corev1.Volume{
		Name: "config",
		VolumeSource: corev1.VolumeSource{
			Secret: &corev1.SecretVolumeSource{
				SecretName: cr.PrefixedName(),
			},
		},
	},
		corev1.Volume{
			Name: "tls-assets",
			VolumeSource: corev1.VolumeSource{
				Secret: &corev1.SecretVolumeSource{
					SecretName: cr.TLSAssetName(),
				},
			},
		},
		corev1.Volume{
			Name: "config-out",
			VolumeSource: corev1.VolumeSource{
				EmptyDir: &corev1.EmptyDirVolumeSource{},
			},
		},
		corev1.Volume{
			Name: "relabeling-assets",
			VolumeSource: corev1.VolumeSource{
				ConfigMap: &corev1.ConfigMapVolumeSource{
					LocalObjectReference: corev1.LocalObjectReference{
						Name: cr.RelabelingAssetName(),
					},
				},
			},
		},
	)

	var agentVolumeMounts []corev1.VolumeMount

	agentVolumeMounts = append(agentVolumeMounts,
		corev1.VolumeMount{
			Name:      vmAgentPersistentQueueMountName,
			MountPath: pqMountPath,
		},
	)
	agentVolumeMounts = append(agentVolumeMounts, cr.Spec.VolumeMounts...)
	agentVolumeMounts = append(agentVolumeMounts,
		corev1.VolumeMount{
			Name:      "config-out",
			ReadOnly:  true,
			MountPath: vmAgentConOfOutDir,
		},
		corev1.VolumeMount{
			Name:      "tls-assets",
			ReadOnly:  true,
			MountPath: tlsAssetsDir,
		},
		corev1.VolumeMount{
			Name:      "relabeling-assets",
			ReadOnly:  true,
			MountPath: RelabelingConfigDir,
		},
	)

	for _, s := range cr.Spec.Secrets {
		volumes = append(volumes, corev1.Volume{
			Name: k8stools.SanitizeVolumeName("secret-" + s),
			VolumeSource: corev1.VolumeSource{
				Secret: &corev1.SecretVolumeSource{
					SecretName: s,
				},
			},
		})
		agentVolumeMounts = append(agentVolumeMounts, corev1.VolumeMount{
			Name:      k8stools.SanitizeVolumeName("secret-" + s),
			ReadOnly:  true,
			MountPath: path.Join(SecretsDir, s),
		})
	}

	for _, c := range cr.Spec.ConfigMaps {
		volumes = append(volumes, corev1.Volume{
			Name: k8stools.SanitizeVolumeName("configmap-" + c),
			VolumeSource: corev1.VolumeSource{
				ConfigMap: &corev1.ConfigMapVolumeSource{
					LocalObjectReference: corev1.LocalObjectReference{
						Name: c,
					},
				},
			},
		})
		agentVolumeMounts = append(agentVolumeMounts, corev1.VolumeMount{
			Name:      k8stools.SanitizeVolumeName("configmap-" + c),
			ReadOnly:  true,
			MountPath: path.Join(ConfigMapsDir, c),
		})
	}

	if cr.Spec.RelabelConfig != nil || len(cr.Spec.InlineRelabelConfig) > 0 {
		args = append(args, "-remoteWrite.relabelConfig="+path.Join(RelabelingConfigDir, globalRelabelingName))
	}

	args = buildArgsForAdditionalPorts(args, cr.Spec.InsertPorts)

	specRes := buildResources(cr.Spec.Resources, config.Resource(c.VMAgentDefault.Resource), c.VMAgentDefault.UseDefaultResources)

	var containsMaxDiskUsage = false
	for arg := range cr.Spec.ExtraArgs {
		if arg == "remoteWrite.maxDiskUsagePerURL" {
			containsMaxDiskUsage = true
		}
		break
	}
	if !containsMaxDiskUsage {
		// limit to 1GB
		args = append(args, "-remoteWrite.maxDiskUsagePerURL=1073741824")
	}
	args = addExtraArgsOverrideDefaults(args, cr.Spec.ExtraArgs, "-")
	sort.Strings(args)

	vmagentContainer := corev1.Container{
		Name:                     "vmagent",
		Image:                    fmt.Sprintf("%s:%s", formatContainerImage(c.ContainerRegistry, cr.Spec.Image.Repository), cr.Spec.Image.Tag),
		ImagePullPolicy:          cr.Spec.Image.PullPolicy,
		Ports:                    ports,
		Args:                     args,
		Env:                      envs,
		VolumeMounts:             agentVolumeMounts,
		Resources:                specRes,
		TerminationMessagePolicy: corev1.TerminationMessageFallbackToLogsOnError,
	}

	vmagentContainer = buildProbe(vmagentContainer, cr)

	configReloader := buildConfigReloaderContainer(cr, c)

	operatorContainers := []corev1.Container{
		configReloader,
		vmagentContainer,
	}

	containers, err := k8stools.MergePatchContainers(operatorContainers, cr.Spec.Containers)
	if err != nil {
		return nil, err
	}

	for i := range cr.Spec.TopologySpreadConstraints {
		if cr.Spec.TopologySpreadConstraints[i].LabelSelector == nil {
			cr.Spec.TopologySpreadConstraints[i].LabelSelector = &metav1.LabelSelector{
				MatchLabels: cr.SelectorLabels(),
			}
		}
	}

	return &corev1.PodSpec{
		NodeSelector:                  cr.Spec.NodeSelector,
		Volumes:                       volumes,
		InitContainers:                cr.Spec.InitContainers,
		Containers:                    containers,
		ServiceAccountName:            cr.GetServiceAccountName(),
		SecurityContext:               cr.Spec.SecurityContext,
		ImagePullSecrets:              cr.Spec.ImagePullSecrets,
		Affinity:                      cr.Spec.Affinity,
		SchedulerName:                 cr.Spec.SchedulerName,
		Tolerations:                   cr.Spec.Tolerations,
		PriorityClassName:             cr.Spec.PriorityClassName,
		HostNetwork:                   cr.Spec.HostNetwork,
		DNSPolicy:                     cr.Spec.DNSPolicy,
		DNSConfig:                     cr.Spec.DNSConfig,
		RuntimeClassName:              cr.Spec.RuntimeClassName,
		HostAliases:                   cr.Spec.HostAliases,
		TopologySpreadConstraints:     cr.Spec.TopologySpreadConstraints,
		TerminationGracePeriodSeconds: cr.Spec.TerminationGracePeriodSeconds,
		ReadinessGates:                cr.Spec.ReadinessGates,
	}, nil
}

func addShardSettingsToVMAgent(shardNum, shardsCount int, dep runtime.Object) {
	var containers []corev1.Container
	switch dep := dep.(type) {
	case *appsv1.StatefulSet:
		containers = dep.Spec.Template.Spec.Containers
		dep.Name = fmt.Sprintf("%s-%d", dep.Name, shardNum)
		// need to mutate selectors ?
		dep.Spec.Selector.MatchLabels["shard-num"] = strconv.Itoa(shardNum)
		dep.Spec.Template.Labels["shard-num"] = strconv.Itoa(shardNum)
	case *appsv1.Deployment:
		containers = dep.Spec.Template.Spec.Containers
		dep.Name = fmt.Sprintf("%s-%d", dep.Name, shardNum)
		// need to mutate selectors ?
		dep.Spec.Selector.MatchLabels["shard-num"] = strconv.Itoa(shardNum)
		dep.Spec.Template.Labels["shard-num"] = strconv.Itoa(shardNum)
	}
	for i := range containers {
		container := &containers[i]
		if container.Name == "vmagent" {
			args := container.Args
			// filter extraArgs defined by user
			cnt := 0
			for i := range args {
				arg := args[i]
				if !strings.Contains(arg, "promscrape.cluster.membersCount") && !strings.Contains(arg, "promscrape.cluster.memberNum") {
					args[cnt] = arg
					cnt++
				}
			}
			args = args[:cnt]
			args = append(args, fmt.Sprintf("-promscrape.cluster.membersCount=%d", shardsCount))
			args = append(args, fmt.Sprintf("-promscrape.cluster.memberNum=%d", shardNum))
			container.Args = args
		}
	}
}

// buildVMAgentRelabelingsAssets combines all possible relabeling config configuration and adding it to the configmap.
func buildVMAgentRelabelingsAssets(ctx context.Context, cr *victoriametricsv1beta1.VMAgent, rclient client.Client) (*corev1.ConfigMap, error) {
	cfgCM := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:       cr.Namespace,
			Name:            cr.RelabelingAssetName(),
			Labels:          cr.AllLabels(),
			Annotations:     cr.AnnotationsFiltered(),
			OwnerReferences: cr.AsOwner(),
		},
		Data: make(map[string]string),
	}
	// global section
	if len(cr.Spec.InlineRelabelConfig) > 0 {
		rcs := addRelabelConfigs(nil, cr.Spec.InlineRelabelConfig)
		data, err := yaml.Marshal(rcs)
		if err != nil {
			return nil, fmt.Errorf("cannot serialize relabelConfig as yaml: %w", err)
		}
		if len(data) > 0 {
			cfgCM.Data[globalRelabelingName] = string(data)
		}
	}
	if cr.Spec.RelabelConfig != nil {
		// need to fetch content from
		data, err := fetchConfigMapContentByKey(ctx, rclient,
			&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: cr.Spec.RelabelConfig.Name, Namespace: cr.Namespace}},
			cr.Spec.RelabelConfig.Key)
		if err != nil {
			return nil, fmt.Errorf("cannot fetch configmap: %s, err: %w", cr.Spec.RelabelConfig.Name, err)
		}
		if len(data) > 0 {
			cfgCM.Data[globalRelabelingName] += data
		}
	}
	// per remoteWrite section.
	for i := range cr.Spec.RemoteWrite {
		rw := cr.Spec.RemoteWrite[i]
		if len(rw.InlineUrlRelabelConfig) > 0 {
			rcs := addRelabelConfigs(nil, rw.InlineUrlRelabelConfig)
			data, err := yaml.Marshal(rcs)
			if err != nil {
				return nil, fmt.Errorf("cannot serialize urlRelabelConfig as yaml: %w", err)
			}
			if len(data) > 0 {
				cfgCM.Data[fmt.Sprintf(urlRelabelingName, i)] = string(data)
			}
		}
		if rw.UrlRelabelConfig != nil {
			data, err := fetchConfigMapContentByKey(ctx, rclient,
				&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: rw.UrlRelabelConfig.Name, Namespace: cr.Namespace}},
				rw.UrlRelabelConfig.Key)
			if err != nil {
				return nil, fmt.Errorf("cannot fetch configmap: %s, err: %w", cr.Spec.RelabelConfig.Name, err)
			}
			if len(data) > 0 {
				cfgCM.Data[fmt.Sprintf(urlRelabelingName, i)] += data
			}
		}
	}
	return cfgCM, nil
}

// CreateOrUpdateRelabelConfigsAssets builds relabeling configs for vmagent at separate configmap, serialized as yaml
func CreateOrUpdateRelabelConfigsAssets(ctx context.Context, cr *victoriametricsv1beta1.VMAgent, rclient client.Client) error {

	assestsCM, err := buildVMAgentRelabelingsAssets(ctx, cr, rclient)
	if err != nil {
		return err
	}
	var existCM corev1.ConfigMap
	if err := rclient.Get(ctx, types.NamespacedName{Namespace: cr.Namespace, Name: cr.RelabelingAssetName()}, &existCM); err != nil {
		if errors.IsNotFound(err) {
			return rclient.Create(ctx, assestsCM)
		}
	}
	assestsCM.Annotations = labels.Merge(existCM.Annotations, assestsCM.Annotations)
	victoriametricsv1beta1.MergeFinalizers(assestsCM, victoriametricsv1beta1.FinalizerName)
	return rclient.Update(ctx, assestsCM)
}

func fetchConfigMapContentByKey(ctx context.Context, rclient client.Client, cm *corev1.ConfigMap, key string) (string, error) {
	if err := rclient.Get(ctx, types.NamespacedName{Namespace: cm.Namespace, Name: cm.Name}, cm); err != nil {
		return "", err
	}
	return cm.Data[key], nil
}

func CreateOrUpdateTlsAssets(ctx context.Context, cr *victoriametricsv1beta1.VMAgent, rclient client.Client) error {
	podScrapes, err := SelectPodScrapes(ctx, cr, rclient)
	if err != nil {
		return fmt.Errorf("cannot select PodScrapes: %w", err)
	}
	scrapes, err := SelectServiceScrapes(ctx, cr, rclient)
	if err != nil {
		return fmt.Errorf("cannot select service scrapes for tls Assets: %w", err)
	}
	nodes, err := SelectVMNodeScrapes(ctx, cr, rclient)
	if err != nil {
		return fmt.Errorf("cannot select nodescrapes for tls Assests: %w", err)
	}
	statics, err := SelectStaticScrapes(ctx, cr, rclient)
	if err != nil {
		return fmt.Errorf("cannot select staticScrapes for tls Assests: %w", err)
	}
	probes, err := SelectVMProbes(ctx, cr, rclient)
	if err != nil {
		return fmt.Errorf("cannot select probecrapes for tls Assests: %w", err)
	}
	assets, err := loadTLSAssets(ctx, rclient, cr, scrapes, podScrapes, probes, nodes, statics)
	if err != nil {
		return fmt.Errorf("cannot load tls assets: %w", err)
	}

	tlsAssetsSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:            cr.TLSAssetName(),
			Labels:          cr.AllLabels(),
			Annotations:     cr.AnnotationsFiltered(),
			OwnerReferences: cr.AsOwner(),
			Namespace:       cr.Namespace,
			Finalizers:      []string{victoriametricsv1beta1.FinalizerName},
		},
		Data: map[string][]byte{},
	}

	for key, asset := range assets {
		tlsAssetsSecret.Data[key] = []byte(asset)
	}
	currentAssetSecret := &corev1.Secret{}
	err = rclient.Get(ctx, types.NamespacedName{Namespace: cr.Namespace, Name: tlsAssetsSecret.Name}, currentAssetSecret)
	if err != nil {
		if errors.IsNotFound(err) {
			log.Info("creating new tls asset for vmagent", "secret_name", tlsAssetsSecret.Name, "vmagent", cr.Name)
			return rclient.Create(ctx, tlsAssetsSecret)
		}
		return fmt.Errorf("cannot get existing tls secret: %s, for vmagent: %s, err: %w", tlsAssetsSecret.Name, cr.Name, err)
	}
	tlsAssetsSecret.Annotations = labels.Merge(currentAssetSecret.Annotations, tlsAssetsSecret.Annotations)
	victoriametricsv1beta1.MergeFinalizers(tlsAssetsSecret, victoriametricsv1beta1.FinalizerName)
	return rclient.Update(ctx, tlsAssetsSecret)
}

func loadTLSAssets(
	ctx context.Context,
	rclient client.Client,
	cr *victoriametricsv1beta1.VMAgent,
	scrapes map[string]*victoriametricsv1beta1.VMServiceScrape,
	podScrapes map[string]*victoriametricsv1beta1.VMPodScrape,
	probes map[string]*victoriametricsv1beta1.VMProbe,
	nodes map[string]*victoriametricsv1beta1.VMNodeScrape,
	statics map[string]*victoriametricsv1beta1.VMStaticScrape,
) (map[string]string, error) {
	assets := map[string]string{}
	nsSecretCache := make(map[string]*corev1.Secret)
	nsConfigMapCache := make(map[string]*corev1.ConfigMap)

	for _, rw := range cr.Spec.RemoteWrite {
		if rw.TLSConfig == nil {
			continue
		}
		if err := addAssetsToCache(ctx, rclient, cr.Namespace, rw.TLSConfig, assets, nsSecretCache, nsConfigMapCache); err != nil {
			return nil, fmt.Errorf("cannot add asset for remote write target: %s,err: %w", cr.Name, err)
		}
	}
	for _, pod := range podScrapes {
		for _, ep := range pod.Spec.PodMetricsEndpoints {
			if ep.VMScrapeParams != nil && ep.VMScrapeParams.ProxyClientConfig != nil && ep.VMScrapeParams.ProxyClientConfig.TLSConfig != nil {
				if err := addAssetsToCache(ctx, rclient, pod.Namespace, ep.VMScrapeParams.ProxyClientConfig.TLSConfig, assets, nsSecretCache, nsConfigMapCache); err != nil {
					return nil, fmt.Errorf("cannot add asset err: %w", err)
				}
			}
			if ep.TLSConfig == nil {
				continue
			}
			if err := addAssetsToCache(ctx, rclient, pod.Namespace, ep.TLSConfig, assets, nsSecretCache, nsConfigMapCache); err != nil {
				return nil, fmt.Errorf("cannot add asset for vmpodscrape: %s,err: %w", pod.Name, err)
			}
		}
	}
	for _, mon := range scrapes {
		for _, ep := range mon.Spec.Endpoints {
			if ep.VMScrapeParams != nil && ep.VMScrapeParams.ProxyClientConfig != nil && ep.VMScrapeParams.ProxyClientConfig.TLSConfig != nil {
				if err := addAssetsToCache(ctx, rclient, mon.Namespace, ep.VMScrapeParams.ProxyClientConfig.TLSConfig, assets, nsSecretCache, nsConfigMapCache); err != nil {
					return nil, fmt.Errorf("cannot add asset err: %w", err)
				}
			}
			if ep.TLSConfig == nil {
				continue
			}
			if err := addAssetsToCache(ctx, rclient, mon.Namespace, ep.TLSConfig, assets, nsSecretCache, nsConfigMapCache); err != nil {
				return nil, fmt.Errorf("cannot add asset for vmservicescrape: %s, err: %w", mon.Name, err)
			}
		}
	}
	for _, probe := range probes {
		if probe.Spec.VMScrapeParams != nil && probe.Spec.VMScrapeParams.ProxyClientConfig != nil && probe.Spec.VMScrapeParams.ProxyClientConfig.TLSConfig != nil {
			if err := addAssetsToCache(ctx, rclient, probe.Namespace, probe.Spec.VMScrapeParams.ProxyClientConfig.TLSConfig, assets, nsSecretCache, nsConfigMapCache); err != nil {
				return nil, fmt.Errorf("cannot add asset err: %w", err)
			}
		}
		if probe.Spec.TLSConfig == nil {
			continue
		}
		if err := addAssetsToCache(ctx, rclient, probe.Namespace, probe.Spec.TLSConfig, assets, nsSecretCache, nsConfigMapCache); err != nil {
			return nil, fmt.Errorf("cannot add asset for vmprobe: %s,err :%w", probe.Name, err)
		}
	}
	for _, staticCfg := range statics {
		for _, ep := range staticCfg.Spec.TargetEndpoints {
			if ep.VMScrapeParams != nil && ep.VMScrapeParams.ProxyClientConfig != nil && ep.VMScrapeParams.ProxyClientConfig.TLSConfig != nil {
				if err := addAssetsToCache(ctx, rclient, staticCfg.Namespace, ep.VMScrapeParams.ProxyClientConfig.TLSConfig, assets, nsSecretCache, nsConfigMapCache); err != nil {
					return nil, fmt.Errorf("cannot add asset err: %w", err)
				}
			}
			if ep.TLSConfig == nil {
				continue
			}
			if err := addAssetsToCache(ctx, rclient, staticCfg.Namespace, ep.TLSConfig, assets, nsSecretCache, nsConfigMapCache); err != nil {
				return nil, fmt.Errorf("cannot add asset for vmservicescrape: %s, err: %w", staticCfg.Name, err)
			}
		}
	}

	for _, node := range nodes {
		if node.Spec.VMScrapeParams != nil && node.Spec.VMScrapeParams.ProxyClientConfig != nil && node.Spec.VMScrapeParams.ProxyClientConfig.TLSConfig != nil {
			if err := addAssetsToCache(ctx, rclient, node.Namespace, node.Spec.VMScrapeParams.ProxyClientConfig.TLSConfig, assets, nsSecretCache, nsConfigMapCache); err != nil {
				return nil, fmt.Errorf("cannot add asset err: %w", err)
			}
		}
		if node.Spec.TLSConfig == nil {
			continue
		}
		if err := addAssetsToCache(ctx, rclient, node.Namespace, node.Spec.TLSConfig, assets, nsSecretCache, nsConfigMapCache); err != nil {
			return nil, fmt.Errorf("cannot add asset for vmnode: %s,err :%w", node.Name, err)
		}
	}
	return assets, nil
}

func addAssetsToCache(
	ctx context.Context,
	rclient client.Client,
	objectNS string,
	tlsConfig *victoriametricsv1beta1.TLSConfig,
	assets map[string]string,
	nsSecretCache map[string]*corev1.Secret,
	nsConfigMapCache map[string]*corev1.ConfigMap,
) error {
	prefix := objectNS + "/"
	secretSelectors := map[string]*corev1.SecretKeySelector{}
	configMapSelectors := map[string]*corev1.ConfigMapKeySelector{}
	if tlsConfig.CA != (victoriametricsv1beta1.SecretOrConfigMap{}) {
		selectorKey := tlsConfig.CA.BuildSelectorWithPrefix(prefix)
		switch {
		case tlsConfig.CA.Secret != nil:
			secretSelectors[selectorKey] = tlsConfig.CA.Secret
		case tlsConfig.CA.ConfigMap != nil:
			configMapSelectors[selectorKey] = tlsConfig.CA.ConfigMap
		}
	}
	if tlsConfig.Cert != (victoriametricsv1beta1.SecretOrConfigMap{}) {
		selectorKey := tlsConfig.Cert.BuildSelectorWithPrefix(prefix)
		switch {
		case tlsConfig.Cert.Secret != nil:
			secretSelectors[selectorKey] = tlsConfig.Cert.Secret
		case tlsConfig.Cert.ConfigMap != nil:
			configMapSelectors[selectorKey] = tlsConfig.Cert.ConfigMap
		}
	}
	if tlsConfig.KeySecret != nil {
		secretSelectors[prefix+tlsConfig.KeySecret.Name+"/"+tlsConfig.KeySecret.Key] = tlsConfig.KeySecret
	}

	for key, selector := range secretSelectors {
		asset, err := getCredFromSecret(
			ctx,
			rclient,
			objectNS,
			selector,
			key,
			nsSecretCache,
		)
		if err != nil {
			return fmt.Errorf(
				"failed to extract endpoint tls asset  from secret %s and key %s in namespace %s",
				selector.Name, selector.Key, objectNS,
			)
		}

		assets[tlsConfig.BuildAssetPath(objectNS, selector.Name, selector.Key)] = asset
	}

	for key, selector := range configMapSelectors {
		asset, err := getCredFromConfigMap(
			ctx,
			rclient,
			objectNS,
			*selector,
			key,
			nsConfigMapCache,
		)
		if err != nil {
			return fmt.Errorf(
				"failed to extract endpoint tls asset from configmap %s and key %s in namespace %s",
				selector.Name, selector.Key, objectNS,
			)
		}

		assets[tlsConfig.BuildAssetPath(objectNS, selector.Name, selector.Key)] = asset
	}
	return nil

}

func LoadRemoteWriteSecrets(ctx context.Context, cr *victoriametricsv1beta1.VMAgent, rclient client.Client) (*scrapesSecretsCache, error) {

	ssCache, err := loadScrapeSecrets(ctx, rclient, nil, nil, nil, nil, nil, nil, cr.Spec.RemoteWrite, cr.Namespace)
	if err != nil {
		return nil, fmt.Errorf("cannot load scrapeSecrets for remoteWriteSpec: %w", err)
	}

	return ssCache, nil
}

type remoteFlag struct {
	isNotNull   bool
	flagSetting string
}

func BuildRemoteWriteSettings(cr *victoriametricsv1beta1.VMAgent) []string {
	if cr.Spec.RemoteWriteSettings == nil {
		return nil
	}
	var args []string
	rws := *cr.Spec.RemoteWriteSettings
	if rws.FlushInterval != nil {
		args = append(args, fmt.Sprintf("-remoteWrite.flushInterval=%s", *rws.FlushInterval))
	}
	if rws.MaxBlockSize != nil {
		args = append(args, fmt.Sprintf("-remoteWrite.maxBlockSize=%d", *rws.MaxBlockSize))
	}
	if rws.MaxDiskUsagePerURL != nil {
		args = append(args, fmt.Sprintf("-remoteWrite.maxDiskUsagePerURL=%d", *rws.MaxDiskUsagePerURL))
	}
	if rws.Queues != nil {
		args = append(args, fmt.Sprintf("-remoteWrite.queues=%d", *rws.Queues))
	}
	if rws.ShowURL != nil {
		args = append(args, fmt.Sprintf("-remoteWrite.showURL=%t", *rws.ShowURL))
	}
	if rws.TmpDataPath != nil {
		args = append(args, fmt.Sprintf("-remoteWrite.tmpDataPath=%s", *rws.TmpDataPath))
	}
	if rws.Labels != nil {
		lbls := sortMap(rws.Labels)
		flagValue := "-remoteWrite.label="
		if len(lbls) > 0 {
			flagValue += fmt.Sprintf("%s=%s", lbls[0].key, lbls[0].value)
			for _, lv := range lbls[1:] {
				flagValue += fmt.Sprintf(",%s=%s", lv.key, lv.value)
			}
			args = append(args, flagValue)
		}

	}
	return args
}

func BuildRemoteWrites(cr *victoriametricsv1beta1.VMAgent, ssCache *scrapesSecretsCache) []string {
	var finalArgs []string
	var remoteArgs []remoteFlag
	remoteTargets := cr.Spec.RemoteWrite

	url := remoteFlag{flagSetting: "-remoteWrite.url=", isNotNull: true}
	if cr.Spec.RemoteWriteSettings != nil && cr.Spec.RemoteWriteSettings.UseMultiTenantMode {
		url = remoteFlag{flagSetting: "-remoteWrite.multitenantURL=", isNotNull: true}
	}
	authUser := remoteFlag{flagSetting: "-remoteWrite.basicAuth.username="}
	authPassword := remoteFlag{flagSetting: "-remoteWrite.basicAuth.password="}
	bearerToken := remoteFlag{flagSetting: "-remoteWrite.bearerToken="}
	urlRelabelConfig := remoteFlag{flagSetting: "-remoteWrite.urlRelabelConfig="}
	sendTimeout := remoteFlag{flagSetting: "-remoteWrite.sendTimeout="}
	tlsCAs := remoteFlag{flagSetting: "-remoteWrite.tlsCAFile="}
	tlsCerts := remoteFlag{flagSetting: "-remoteWrite.tlsCertFile="}
	tlsKeys := remoteFlag{flagSetting: "-remoteWrite.tlsKeyFile="}
	tlsInsecure := remoteFlag{flagSetting: "-remoteWrite.tlsInsecureSkipVerify="}
	tlsServerName := remoteFlag{flagSetting: "-remoteWrite.tlsServerName="}
	oauth2ClientID := remoteFlag{flagSetting: "-remoteWrite.oauth2.clientID="}
	oauth2ClientSecret := remoteFlag{flagSetting: "-remoteWrite.oauth2.clientSecret="}
	oauth2ClientSecretFile := remoteFlag{flagSetting: "-remoteWrite.oauth2.clientSecretFile="}
	oauth2Scopes := remoteFlag{flagSetting: "-remoteWrite.oauth2.scopes="}
	oauth2TokenUrl := remoteFlag{flagSetting: "-remoteWrite.oauth2.tokenUrl="}
	headers := remoteFlag{flagSetting: "-remoteWrite.headers="}

	pathPrefix := path.Join(tlsAssetsDir, cr.Namespace)

	for i := range remoteTargets {

		rws := remoteTargets[i]
		url.flagSetting += fmt.Sprintf("%s,", rws.URL)

		var caPath, certPath, keyPath, ServerName string
		var insecure bool
		if rws.TLSConfig != nil {
			if rws.TLSConfig.CAFile != "" {
				caPath = rws.TLSConfig.CAFile
			} else if rws.TLSConfig.CA.Name() != "" {
				caPath = rws.TLSConfig.BuildAssetPath(pathPrefix, rws.TLSConfig.CA.Name(), rws.TLSConfig.CA.Key())
			}
			if caPath != "" {
				tlsCAs.isNotNull = true
			}
			if rws.TLSConfig.CertFile != "" {
				certPath = rws.TLSConfig.CertFile
			} else if rws.TLSConfig.Cert.Name() != "" {
				certPath = rws.TLSConfig.BuildAssetPath(pathPrefix, rws.TLSConfig.Cert.Name(), rws.TLSConfig.Cert.Key())
			}
			if certPath != "" {
				tlsCerts.isNotNull = true
			}
			switch {
			case rws.TLSConfig.KeyFile != "":
				keyPath = rws.TLSConfig.KeyFile
			case rws.TLSConfig.KeySecret != nil:
				keyPath = rws.TLSConfig.BuildAssetPath(pathPrefix, rws.TLSConfig.KeySecret.Name, rws.TLSConfig.KeySecret.Key)
			}
			if keyPath != "" {
				tlsKeys.isNotNull = true
			}
			if rws.TLSConfig.InsecureSkipVerify {
				tlsInsecure.isNotNull = true
			}
			if rws.TLSConfig.ServerName != "" {
				ServerName = rws.TLSConfig.ServerName
				tlsServerName.isNotNull = true
			}
			insecure = rws.TLSConfig.InsecureSkipVerify
		}
		tlsCAs.flagSetting += fmt.Sprintf("%s,", caPath)
		tlsCerts.flagSetting += fmt.Sprintf("%s,", certPath)
		tlsKeys.flagSetting += fmt.Sprintf("%s,", keyPath)
		tlsServerName.flagSetting += fmt.Sprintf("%s,", ServerName)
		tlsInsecure.flagSetting += fmt.Sprintf("%v,", insecure)

		var user string
		var pass string
		if rws.BasicAuth != nil {
			if s, ok := ssCache.baSecrets[fmt.Sprintf("remoteWriteSpec/%s", rws.URL)]; ok {
				authUser.isNotNull = true
				authPassword.isNotNull = true
				user = s.username
				pass = s.password
			}
		}
		authUser.flagSetting += fmt.Sprintf("\"%s\",", strings.ReplaceAll(user, `"`, `\"`))
		authPassword.flagSetting += fmt.Sprintf("\"%s\",", strings.ReplaceAll(pass, `"`, `\"`))

		var value string
		if rws.BearerTokenSecret != nil && rws.BearerTokenSecret.Name != "" {
			if s, ok := ssCache.bearerTokens[fmt.Sprintf("remoteWriteSpec/%s", rws.URL)]; ok {
				bearerToken.isNotNull = true
				value = s
			}
		}
		bearerToken.flagSetting += fmt.Sprintf("\"%s\",", strings.ReplaceAll(value, `"`, `\"`))

		value = ""

		if rws.UrlRelabelConfig != nil || len(rws.InlineUrlRelabelConfig) > 0 {
			urlRelabelConfig.isNotNull = true
			value = path.Join(RelabelingConfigDir, fmt.Sprintf(urlRelabelingName, i))
		}

		urlRelabelConfig.flagSetting += fmt.Sprintf("%s,", value)

		value = ""
		if rws.SendTimeout != nil {
			if !sendTimeout.isNotNull {
				sendTimeout.isNotNull = true
			}
			value = *rws.SendTimeout
		}
		sendTimeout.flagSetting += fmt.Sprintf("%s,", value)

		value = ""
		if len(rws.Headers) > 0 {
			headers.isNotNull = true
			for _, headerValue := range rws.Headers {
				value += headerValue + "^^"
			}
			value = strings.TrimSuffix(value, "^^")
		}
		headers.flagSetting += fmt.Sprintf("'%s',", value)
		value = ""
		var oaturl, oascopes, oaclientID, oaSecretKey, oaSecretKeyFile string
		if rws.OAuth2 != nil {
			if len(rws.OAuth2.TokenURL) > 0 {
				oauth2TokenUrl.isNotNull = true
				oaturl = rws.OAuth2.TokenURL
			}

			if len(rws.OAuth2.Scopes) > 0 {
				oauth2Scopes.isNotNull = true
				oascopes = strings.Join(rws.OAuth2.Scopes, ",")
			}

			if len(rws.OAuth2.ClientSecretFile) > 0 {
				oauth2ClientSecretFile.isNotNull = true
				oaSecretKeyFile = rws.OAuth2.ClientSecretFile
			}

			sv := ssCache.oauth2Secrets[fmt.Sprintf("remoteWriteSpec/%s", rws.URL)]
			if rws.OAuth2.ClientSecret != nil && sv != nil {
				oaSecretKey = sv.clientSecret
				oauth2ClientSecret.isNotNull = true
			}

			if len(rws.OAuth2.ClientID.Name()) > 0 && sv != nil {
				oaclientID = sv.clientID
				oauth2ClientID.isNotNull = true
			}

		}
		oauth2TokenUrl.flagSetting += fmt.Sprintf("%s,", oaturl)
		oauth2ClientSecretFile.flagSetting += fmt.Sprintf("%s,", oaSecretKeyFile)
		oauth2ClientSecret.flagSetting += fmt.Sprintf("%s,", oaSecretKey)
		oauth2ClientID.flagSetting += fmt.Sprintf("%s,", oaclientID)
		oauth2Scopes.flagSetting += fmt.Sprintf("%s,", oascopes)
	}
	remoteArgs = append(remoteArgs, url, authUser, authPassword, bearerToken, urlRelabelConfig, tlsInsecure, sendTimeout)
	remoteArgs = append(remoteArgs, tlsServerName, tlsKeys, tlsCerts, tlsCAs)
	remoteArgs = append(remoteArgs, oauth2ClientID, oauth2ClientSecret, oauth2ClientSecretFile, oauth2Scopes, oauth2TokenUrl)
	remoteArgs = append(remoteArgs, headers)

	for _, remoteArgType := range remoteArgs {
		if remoteArgType.isNotNull {
			finalArgs = append(finalArgs, strings.TrimSuffix(remoteArgType.flagSetting, ","))
		}
	}
	return finalArgs
}

func buildConfigReloaderContainer(cr *victoriametricsv1beta1.VMAgent, c *config.BaseOperatorConf) corev1.Container {

	configReloadVolumeMounts := []corev1.VolumeMount{
		{
			Name:      "config",
			MountPath: vmAgentConfDir,
		},
		{
			Name:      "config-out",
			MountPath: vmAgentConOfOutDir,
		},
		{
			Name:      "relabeling-assets",
			ReadOnly:  true,
			MountPath: RelabelingConfigDir,
		},
	}
	configReloaderResources := corev1.ResourceRequirements{
		Limits: corev1.ResourceList{}, Requests: corev1.ResourceList{}}
	if c.VMAgentDefault.ConfigReloaderCPU != "0" && c.VMAgentDefault.UseDefaultResources {
		configReloaderResources.Limits[corev1.ResourceCPU] = resource.MustParse(c.VMAgentDefault.ConfigReloaderCPU)
	}
	if c.VMAgentDefault.ConfigReloaderMemory != "0" && c.VMAgentDefault.UseDefaultResources {
		configReloaderResources.Limits[corev1.ResourceMemory] = resource.MustParse(c.VMAgentDefault.ConfigReloaderMemory)
	}

	configReloadArgs := buildConfigReloaderArgs(cr, c)
	cntr := corev1.Container{
		Name:                     "config-reloader",
		Image:                    formatContainerImage(c.ContainerRegistry, c.VMAgentDefault.ConfigReloadImage),
		TerminationMessagePolicy: corev1.TerminationMessageFallbackToLogsOnError,
		Env: []corev1.EnvVar{
			{
				Name: "POD_NAME",
				ValueFrom: &corev1.EnvVarSource{
					FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.name"},
				},
			},
		},
		Command:      []string{"/bin/prometheus-config-reloader"},
		Args:         configReloadArgs,
		VolumeMounts: configReloadVolumeMounts,
		Resources:    configReloaderResources,
	}
	if c.UseCustomConfigReloader {
		cntr.Image = fmt.Sprintf("%s", formatContainerImage(c.ContainerRegistry, c.CustomConfigReloaderImage))
		cntr.Command = []string{"/usr/local/bin/config-reloader"}
	}
	return cntr
}

func buildConfigReloaderArgs(cr *victoriametricsv1beta1.VMAgent, c *config.BaseOperatorConf) []string {

	// by default use watched-dir
	// it should simplify parsing for latest and empty version tags.
	dirsArg := "watched-dir"
	if !c.UseCustomConfigReloader {
		reloaderImage := c.VMAgentDefault.ConfigReloadImage
		idx := strings.LastIndex(reloaderImage, ":")
		if idx > 0 {
			imageTag := reloaderImage[idx+1:]
			ver, err := version.NewVersion(imageTag)
			if err != nil {
				log.Error(err, "cannot parse vmagent config reloader version", "reloader-image", reloaderImage)
			} else if ver.LessThan(version.Must(version.NewVersion("0.43.0"))) {
				dirsArg = "rules-dir"
			}
		}
	}

	args := []string{
		fmt.Sprintf("--reload-url=%s", cr.ReloadPathWithPort(cr.Spec.Port)),
		fmt.Sprintf("--config-envsubst-file=%s", path.Join(vmAgentConOfOutDir, configEnvsubstFilename)),
		fmt.Sprintf("--%s=%s", dirsArg, RelabelingConfigDir),
	}
	if c.UseCustomConfigReloader {
		args = append(args, fmt.Sprintf("--config-secret-name=%s/%s", cr.Namespace, cr.PrefixedName()))
		args = append(args, "--config-secret-key=vmagent.yaml.gz")
	} else {
		args = append(args, fmt.Sprintf("--config-file=%s", path.Join(vmAgentConfDir, configFilename)))
	}
	return args
}
