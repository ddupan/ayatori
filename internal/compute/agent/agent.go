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
	"sync/atomic"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	toolscache "k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	computev1alpha1 "go.ddupan.top/ayatori/api/compute/v1alpha1"
	"go.ddupan.top/ayatori/internal/compute/adapter/libvirt"
	"go.ddupan.top/ayatori/internal/compute/agent/state"
)

const (
	kindNetwork = "LibvirtNetwork"
	kindPool    = "LibvirtStoragePool"
	kindDomain  = "LibvirtDomain"
)

// Config 是节点部署写入的本地配置。
type Config struct {
	NodeName string `json:"nodeName"`
	// Kubeconfig 是访问 Ayatori apiserver 的凭据。
	// ponytail: 先用 kubeconfig；SPIFFE JWT-SVID 登录待 apiserver 的 structured authentication 就绪（ADR-0014）。
	Kubeconfig string `json:"kubeconfig"`
	StateDir   string `json:"stateDir"`
	// LibvirtURI 缺省为 qemu:///system。
	LibvirtURI string `json:"libvirtURI"`
	// HostReservedMemory 是宿主机自身的预留，从 Node 的 allocatable 中扣除（ADR-0015 资源模型）。
	HostReservedMemory resource.Quantity `json:"hostReservedMemory"`
	// ResyncInterval 是没有变化时重新收敛的间隔，也是发现带外修改的最长延迟。
	ResyncInterval metav1.Duration `json:"resyncInterval"`
}

// LoadConfig 读取配置并补默认值。
func LoadConfig(path string) (Config, error) {
	data, err := os.ReadFile(path)
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
	if c.Kubeconfig == "" {
		errs = append(errs, errors.New("kubeconfig is required"))
	}
	if c.StateDir == "" {
		errs = append(errs, errors.New("stateDir is required"))
	}
	return c, errors.Join(errs...)
}

type Agent struct {
	config Config
	store  *state.Store
	hv     *libvirt.Client
	rest   *rest.Config
	scheme *runtime.Scheme
	kube   client.Client
	logger *slog.Logger

	// observed 是最近一轮 reconcile 的结果，reported 是已经写入 API 的结果；两者不同时才写。
	observed map[string]observation
	reported map[string]observation
	// connected 表示 libvirt 是否连通，写入 Node 的 Ready condition。
	connected bool
	facts     atomic.Pointer[nodeFacts]
}

// New 只加载本地状态并构造客户端，不访问 API：控制面不可用时 Agent 也必须能启动。
func New(config Config, logger *slog.Logger) (*Agent, error) {
	store, err := state.Open(config.StateDir)
	if err != nil {
		return nil, err
	}
	restConfig, err := clientcmd.BuildConfigFromFlags("", config.Kubeconfig)
	if err != nil {
		return nil, err
	}
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		return nil, err
	}
	if err := computev1alpha1.AddToScheme(scheme); err != nil {
		return nil, err
	}
	kube, err := client.New(restConfig, client.Options{Scheme: scheme})
	if err != nil {
		return nil, err
	}
	a := &Agent{config: config, store: store, rest: restConfig, scheme: scheme, kube: kube, logger: logger,
		observed: map[string]observation{}, reported: map[string]observation{}}
	a.facts.Store(&nodeFacts{})
	return a, nil
}

// Run 先按本地状态收敛一轮，再开始 watch API；之后在本地状态变化或到达间隔时收敛。
func (a *Agent) Run(ctx context.Context) {
	a.reconcile()
	go a.watch(ctx)
	go a.heartbeat(ctx)
	ticker := time.NewTicker(a.config.ResyncInterval.Duration)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			if a.hv != nil {
				_ = a.hv.Close()
			}
			return
		case <-a.store.Changed():
		case <-ticker.C:
		}
		a.reconcile()
		a.report(ctx)
	}
}

// watch 把 API 中属于本节点的对象写入本地状态。首次同步完成之前不做任何删除判断：
// 只有拿到 API 的完整结果，"对象已不存在"才是事实。
func (a *Agent) watch(ctx context.Context) {
	node := a.config.NodeName
	c, err := cache.New(a.rest, cache.Options{Scheme: a.scheme, ByObject: map[client.Object]cache.ByObject{
		&computev1alpha1.LibvirtDomain{}: {Field: fields.OneTermEqualSelector("spec.nodeName", node)},
		&corev1.Node{}:                   {Field: fields.OneTermEqualSelector("metadata.name", node)},
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
	for _, obj := range []client.Object{&computev1alpha1.LibvirtNetwork{}, &computev1alpha1.LibvirtStoragePool{}, &computev1alpha1.LibvirtDomain{}, &corev1.Node{}} {
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
	var node corev1.Node
	if err := c.Get(ctx, client.ObjectKey{Name: a.config.NodeName}, &node); client.IgnoreNotFound(err) != nil {
		return err
	}
	var nets computev1alpha1.LibvirtNetworkList
	var pools computev1alpha1.LibvirtStoragePoolList
	var domains computev1alpha1.LibvirtDomainList
	if err := c.List(ctx, &nets); err != nil {
		return err
	}
	if err := c.List(ctx, &pools); err != nil {
		return err
	}
	if err := c.List(ctx, &domains); err != nil {
		return err
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
		return labels.SelectorFromSet(selector).Matches(labels.Set(node.Labels))
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
	for i := range domains.Items {
		keep(kindDomain, &domains.Items[i])
	}
	for _, kind := range []string{kindNetwork, kindPool, kindDomain} {
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
