// Package agent 是计算节点上的 Compute Agent（ADR-0015）。链路为 informer → 本地状态 → 本地 reconcile
// → libvirt：本地 reconcile 只读本地状态，控制面不可用时照样按它收敛；删除只在 API 确认后发生。
package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	metav1validation "k8s.io/apimachinery/pkg/apis/meta/v1/validation"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/validation/field"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	toolscache "k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	computev1alpha1 "go.ddupan.top/ayatori/api/compute/v1alpha1"
	"go.ddupan.top/ayatori/internal/compute/adapter/libvirt"
	"go.ddupan.top/ayatori/internal/compute/adapter/lxc"
	"go.ddupan.top/ayatori/internal/compute/agent/state"
	"go.ddupan.top/ayatori/internal/infra/clientcert"
	"go.ddupan.top/ayatori/internal/infra/node"
)

const (
	kindNetwork   = "LibvirtNetwork"
	kindPool      = "LibvirtStoragePool"
	kindVolume    = "LibvirtStorageVolume"
	kindDomain    = "LibvirtDomain"
	kindImage     = "Image"
	kindContainer = "LXCContainer"

	// downloadTimeout 是一次镜像下载的总时长上限。
	downloadTimeout = 2 * time.Hour
)

// kinds 是收敛顺序：被引用的在前。
var kinds = []string{kindNetwork, kindPool, kindImage, kindVolume, kindDomain, kindContainer}

// Config 是节点部署写入的本地配置。
type Config struct {
	NodeName string `json:"nodeName"`
	// CredentialsDir 是 Agent 的客户端证书身份目录（ADR-0021）：部署方写入 bootstrap-kubeconfig，
	// Agent 以 system:ayatori:node:<nodeName> 申请证书并自行续签。节点上使用它。
	CredentialsDir string `json:"credentialsDir"`
	// Kubeconfig 与 CredentialsDir 二选一，用于开发与测试。
	Kubeconfig string `json:"kubeconfig"`
	StateDir   string `json:"stateDir"`
	// LibvirtURI 缺省为 qemu:///system。
	LibvirtURI string `json:"libvirtURI"`
	// HostReservedMemory 是宿主机自身的预留，从 Node 的 allocatable 中扣除（ADR-0015 资源模型）。
	HostReservedMemory resource.Quantity `json:"hostReservedMemory"`
	// NodeLabels 是发现不了、按节点配置的 Node label，例如 topology.kubernetes.io/zone，相当于
	// kubelet 的 --node-labels（ADR-0019 §4）。与发现的 label 同名时以它为准；角色 label 不能在此设置。
	NodeLabels map[string]string `json:"nodeLabels"`
	// MetricsAddress 是 /metrics 的明文监听地址，例如 :9810；为空时不监听。
	MetricsAddress string `json:"metricsAddress"`
	// ResyncInterval 是没有变化时重新收敛的间隔，也是发现带外修改的最长延迟。
	ResyncInterval metav1.Duration `json:"resyncInterval"`
}

// LoadConfig 读取配置并补默认值。
func LoadConfig(path string) (Config, error) {
	data, err := os.ReadFile(path) //nolint:gosec // 路径来自启动参数，读取它正是本函数的职责
	if err != nil {
		return Config{}, err
	}
	var c Config
	if err := yaml.UnmarshalStrict(data, &c); err != nil {
		return Config{}, fmt.Errorf("parse %s: %w", path, err)
	}
	if c.LibvirtURI == "" {
		c.LibvirtURI = "qemu:///system"
	}
	if c.ResyncInterval.Duration == 0 {
		c.ResyncInterval.Duration = 30 * time.Second
	}
	var errs []error
	if c.NodeName == "" {
		errs = append(errs, errors.New("nodeName is required"))
	}
	if (c.CredentialsDir == "") == (c.Kubeconfig == "") {
		errs = append(errs, errors.New("exactly one of credentialsDir and kubeconfig is required"))
	}
	if c.StateDir == "" {
		errs = append(errs, errors.New("stateDir is required"))
	}
	for k := range c.NodeLabels {
		if strings.HasPrefix(k, node.RoleLabelPrefix) {
			errs = append(errs, fmt.Errorf("nodeLabels: %s is a role label", k))
		}
	}
	errs = append(errs, metav1validation.ValidateLabels(c.NodeLabels, field.NewPath("nodeLabels")).ToAggregate())
	return c, errors.Join(errs...)
}

type Agent struct {
	// Containers 是 LXC 运行时，New 设置为 liblxc；测试可以在 Run 之前替换。
	Containers ContainerRuntime

	config Config
	store  *state.Store
	hv     *libvirt.Client
	scheme *runtime.Scheme
	logger *slog.Logger
	// restConfig 读取访问 API 的配置。用证书身份时，证书签发之前它会失败。
	restConfig func() (*rest.Config, error)
	// kube 在拿到 API 配置之后才设置；之前对 API 的写入跳过，留到下一轮。
	kube atomic.Pointer[client.Client]
	// stopCredentials 停止证书续签；使用 Kubeconfig 时为空操作。
	stopCredentials func()

	// observed 是最近一轮 reconcile 的结果，reported 是已经写入 API 的结果；两者不同时才写。
	observed map[string]observation
	reported map[string]observation
	// connected 表示 libvirt 是否连通，写入 Node 的 compute.ayatori.ddupan.top/Ready（只表示 compute 角色，不是 Node Ready）。
	connected bool
	facts     atomic.Pointer[nodeFacts]

	metrics *metrics
	// succeeded 是各对象最近一次成功收敛的时间，seen 是上一轮出现过的对象；只由主循环访问。
	succeeded map[objectID]time.Time
	seen      map[objectID]bool
	// downloads 是进行中的镜像下载，按 Image 名字；只由主循环访问。
	downloads map[string]*download
}

// New 只加载本地状态，不访问 API：控制面不可用、证书尚未签发时 Agent 也必须能启动。证书在后台
// 申请与续签（ADR-0021）。
func New(config Config, registry prometheus.Registerer, logger *slog.Logger) (*Agent, error) {
	store, err := state.Open(config.StateDir)
	if err != nil {
		return nil, err
	}
	stopCredentials := func() {}
	restConfig := func() (*rest.Config, error) { return clientcmd.BuildConfigFromFlags("", config.Kubeconfig) }
	if config.CredentialsDir != "" {
		identity := node.Identity(config.CredentialsDir, config.NodeName, Role)
		if stopCredentials, err = clientcert.Start(context.Background(), identity); err != nil {
			return nil, err
		}
		restConfig = func() (*rest.Config, error) { return clientcert.RESTConfig(config.CredentialsDir) }
	}
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		return nil, err
	}
	if err := computev1alpha1.AddToScheme(scheme); err != nil {
		return nil, err
	}
	a := &Agent{Containers: lxc.New(), config: config, store: store, scheme: scheme, logger: logger, restConfig: restConfig,
		stopCredentials: stopCredentials,
		observed:        map[string]observation{}, reported: map[string]observation{},
		metrics: newMetrics(registry), succeeded: map[objectID]time.Time{}, seen: map[objectID]bool{},
		downloads: map[string]*download{}}
	a.facts.Store(&nodeFacts{})
	return a, nil
}

// Run 先按本地状态收敛一轮，再开始 watch API；之后在本地状态变化或到达间隔时收敛。
func (a *Agent) Run(ctx context.Context) {
	a.reconcile()
	go a.connectAPI(ctx)
	ticker := time.NewTicker(a.config.ResyncInterval.Duration)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			for name := range a.downloads {
				a.stopDownload(name)
			}
			if a.hv != nil {
				_ = a.hv.Close()
			}
			a.stopCredentials()
			return
		case <-a.store.Changed():
		case <-ticker.C:
		}
		a.reconcile()
		a.report(ctx)
	}
}

// connectAPI 等到能构造 API 配置（证书已签发）后，开始 watch 与心跳。
func (a *Agent) connectAPI(ctx context.Context) {
	var config *rest.Config
	for {
		var err error
		if config, err = a.restConfig(); err == nil {
			break
		}
		a.logger.Debug("API credentials not ready", "error", err)
		select {
		case <-ctx.Done():
			return
		case <-time.After(5 * time.Second):
		}
	}
	kube, err := client.New(config, client.Options{Scheme: a.scheme})
	if err != nil {
		a.logger.Error("build API client", "error", err)
		return
	}
	a.kube.Store(&kube)
	go func() {
		// watch 只在建立失败时返回，例如升级时 Agent 先于 CRD 到达。
		for ctx.Err() == nil {
			a.watch(ctx, config)
			select {
			case <-ctx.Done():
			case <-time.After(5 * time.Second):
			}
		}
	}()
	a.heartbeat(ctx, kube)
}

// watch 把 API 中属于本节点的对象写入本地状态。首次同步完成之前不做任何删除判断：
// 只有拿到 API 的完整结果，"对象已不存在"才是事实。
func (a *Agent) watch(ctx context.Context, config *rest.Config) {
	self := a.config.NodeName
	c, err := cache.New(config, cache.Options{Scheme: a.scheme, ByObject: map[client.Object]cache.ByObject{
		&computev1alpha1.LibvirtStorageVolume{}: {Field: fields.OneTermEqualSelector("spec.nodeName", self)},
		&computev1alpha1.LibvirtDomain{}:        {Field: fields.OneTermEqualSelector("spec.nodeName", self)},
		&computev1alpha1.LXCContainer{}:         {Field: fields.OneTermEqualSelector("spec.nodeName", self)},
		&corev1.Node{}:                          {Field: fields.OneTermEqualSelector("metadata.name", self)},
	}})
	if err != nil {
		a.logger.Error("build API cache", "error", err)
		return
	}
	changed := make(chan struct{}, 1)
	notify := toolscache.ResourceEventHandlerFuncs{
		AddFunc:    func(any) { signal(changed) },
		UpdateFunc: func(any, any) { signal(changed) },
		DeleteFunc: func(any) { signal(changed) },
	}
	for _, obj := range []client.Object{&computev1alpha1.LibvirtNetwork{}, &computev1alpha1.LibvirtStoragePool{},
		&computev1alpha1.LibvirtStorageVolume{}, &computev1alpha1.LibvirtDomain{}, &computev1alpha1.LXCContainer{}, &computev1alpha1.Image{}, &corev1.Node{}} {
		informer, err := c.GetInformer(ctx, obj)
		if err != nil {
			a.logger.Error("get informer", "error", err)
			return
		}
		if _, err := informer.AddEventHandler(notify); err != nil {
			a.logger.Error("add event handler", "error", err)
			return
		}
	}
	go func() {
		if err := c.Start(ctx); err != nil {
			a.logger.Error("API cache stopped", "error", err)
		}
	}()
	if !c.WaitForCacheSync(ctx) {
		return
	}
	a.logger.Info("API cache synced")
	for {
		if err := a.sync(ctx, c); err != nil {
			a.logger.Error("sync local state from API", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-changed:
		}
	}
}

// sync 以 API 为准更新本地状态：属于本节点的对象写入，已不属于本节点（删除，或 nodeSelector、
// nodeName 不再匹配）的标记删除。
func (a *Agent) sync(ctx context.Context, c client.Reader) error {
	var self corev1.Node
	if err := c.Get(ctx, client.ObjectKey{Name: a.config.NodeName}, &self); client.IgnoreNotFound(err) != nil {
		return err
	}
	var nets computev1alpha1.LibvirtNetworkList
	var pools computev1alpha1.LibvirtStoragePoolList
	var volumes computev1alpha1.LibvirtStorageVolumeList
	var domains computev1alpha1.LibvirtDomainList
	var containers computev1alpha1.LXCContainerList
	var images computev1alpha1.ImageList
	for _, list := range []client.ObjectList{&nets, &pools, &volumes, &domains, &containers, &images} {
		if err := c.List(ctx, list); err != nil {
			return err
		}
	}
	var errs []error
	selected := map[string]bool{}
	keep := func(kind string, obj client.Object) {
		key := client.ObjectKeyFromObject(obj).String()
		if obj.GetNamespace() == "" {
			key = obj.GetName()
		}
		selected[kind+"/"+key] = true
		obj.SetManagedFields(nil)
		data, err := json.Marshal(obj)
		if err == nil {
			_, err = a.store.Put(kind, key, string(obj.GetUID()), obj.GetResourceVersion(), data)
		}
		errs = append(errs, err)
	}
	matches := func(selector map[string]string) bool {
		return labels.SelectorFromSet(selector).Matches(labels.Set(self.Labels))
	}
	for i := range nets.Items {
		if matches(nets.Items[i].Spec.NodeSelector) {
			keep(kindNetwork, &nets.Items[i])
		}
	}
	for i := range pools.Items {
		if matches(pools.Items[i].Spec.NodeSelector) {
			keep(kindPool, &pools.Items[i])
		}
	}
	for i := range images.Items {
		if matches(images.Items[i].Spec.NodeSelector) {
			keep(kindImage, &images.Items[i])
		}
	}
	for i := range volumes.Items {
		keep(kindVolume, &volumes.Items[i])
	}
	for i := range domains.Items {
		keep(kindDomain, &domains.Items[i])
	}
	for i := range containers.Items {
		keep(kindContainer, &containers.Items[i])
	}
	for _, kind := range kinds {
		for _, r := range a.store.List(kind) {
			if !selected[kind+"/"+r.Key] {
				errs = append(errs, a.store.MarkDeleted(kind, r.Key))
			}
		}
	}
	return errors.Join(errs...)
}

func signal(ch chan struct{}) {
	select {
	case ch <- struct{}{}:
	default:
	}
}
