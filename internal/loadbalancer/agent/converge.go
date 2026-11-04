package agent

import (
	"cmp"
	"context"
	"fmt"
	"net/http"
	"os"
	"slices"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/rest"
	toolscache "k8s.io/client-go/tools/cache"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"

	networkv1alpha1 "git.ddupan.top/panxiao81/ayatori/api/network/v1alpha1"
	"git.ddupan.top/panxiao81/ayatori/internal/loadbalancer/adapter/haproxy"
	"git.ddupan.top/panxiao81/ayatori/internal/loadbalancer/controller"
	"git.ddupan.top/panxiao81/ayatori/internal/loadbalancer/domain/address"
	"git.ddupan.top/panxiao81/ayatori/internal/loadbalancer/domain/loadbalancer"
)

const (
	// ConvergeInterval 是没有事件时重新观察本机代理的间隔：发现带外修改，跟进 reload 与健康检查。
	ConvergeInterval = 5 * time.Second
	writeTimeout     = 10 * time.Second
)

var reloadNames = map[loadbalancer.Reload]string{
	loadbalancer.ReloadSucceeded: "Succeeded", loadbalancer.ReloadPending: "Pending", loadbalancer.ReloadFailed: "Failed",
}

// converge watch LoadBalancer 与 Backend，在本机 HAProxy 上收敛（ADR-0022）。期望配置与 controller
// 远程收敛时相同（controller.Desired），每轮每个 LoadBalancer 至多一次写入。只有 informer 完成首次
// 同步后才判断「对象已不存在」：API 不可达期间缓存不变，不会清理任何配置。
func (a *Agent) converge(ctx context.Context, config *rest.Config, scheme *runtime.Scheme) {
	password, err := os.ReadFile(a.config.DataPlane.PasswordFile)
	if err != nil {
		a.logger.Error("read Data Plane API password", "error", err)
		return
	}
	hosts := &haproxy.Hosts{
		HTTP: &http.Client{}, Username: a.config.DataPlane.Username, Password: strings.TrimSpace(string(password)),
		ConnectTimeout: a.config.DataPlane.ConnectTimeout.Duration, Resolvers: a.config.DataPlane.Resolvers,
	}
	host := loadbalancer.Host{Name: a.config.NodeName, ProxyURL: a.config.DataPlane.URL}
	objects, err := cache.New(config, cache.Options{Scheme: scheme})
	if err != nil {
		a.logger.Error("build API cache", "error", err)
		return
	}
	changed := make(chan struct{}, 1)
	for _, object := range []client.Object{&networkv1alpha1.LoadBalancer{}, &networkv1alpha1.Backend{}} {
		informer, err := objects.GetInformer(ctx, object)
		if err != nil {
			a.logger.Error("watch", "error", err)
			return
		}
		notify := func(any) { signal(changed) }
		if _, err := informer.AddEventHandler(toolscache.ResourceEventHandlerFuncs{
			AddFunc: notify, DeleteFunc: notify, UpdateFunc: func(any, any) { signal(changed) },
		}); err != nil {
			a.logger.Error("watch", "error", err)
			return
		}
	}
	go func() { _ = objects.Start(ctx) }()
	if !objects.WaitForCacheSync(ctx) {
		return
	}
	ticker := time.NewTicker(ConvergeInterval)
	defer ticker.Stop()
	for {
		reports, err := a.convergeOnce(ctx, objects, hosts, host)
		if err != nil {
			a.logger.Warn("本机收敛失败", "error", err)
		} else {
			a.mu.Lock()
			a.loadBalancers = reports
			a.mu.Unlock()
		}
		select {
		case <-ctx.Done():
			return
		case <-changed:
		case <-ticker.C:
		}
	}
}

// convergeOnce 收敛全部 LoadBalancer 一轮，再删除本机上已不存在的 LoadBalancer 的配置。
func (a *Agent) convergeOnce(ctx context.Context, reader client.Reader, hosts *haproxy.Hosts, host loadbalancer.Host) ([]networkv1alpha1.HostLoadBalancerStatus, error) {
	list := &networkv1alpha1.LoadBalancerList{}
	if err := reader.List(ctx, list); err != nil {
		return nil, err
	}
	reports := make([]networkv1alpha1.HostLoadBalancerStatus, 0, len(list.Items))
	live := map[string]bool{}
	for i := range list.Items {
		object := &list.Items[i]
		live[string(object.UID)] = true
		desired, err := controller.Desired(ctx, reader, object)
		if err != nil {
			return nil, err
		}
		reports = append(reports, a.convergeOne(ctx, hosts, host, object, loadbalancer.Reconstitute(desired)))
	}
	ctx, cancel := context.WithTimeout(ctx, writeTimeout)
	defer cancel()
	owners, err := hosts.Owners(ctx, host)
	if err != nil {
		return nil, err
	}
	for _, uid := range owners {
		if !live[uid] {
			a.logger.Info("删除已不存在的 LoadBalancer 的配置", "uid", uid)
			orphan := loadbalancer.Reconstitute(loadbalancer.Snapshot{Owner: address.Owner{UID: uid}})
			if err := hosts.Remove(ctx, host, orphan); err != nil {
				return nil, err
			}
		}
	}
	slices.SortFunc(reports, func(x, y networkv1alpha1.HostLoadBalancerStatus) int {
		return cmp.Or(strings.Compare(x.Namespace, y.Namespace), strings.Compare(x.Name, y.Name))
	})
	return reports, nil
}

// convergeOne 观察并至多写入一次，返回本轮观察到的事实；写入的结果由下一轮观察。
func (a *Agent) convergeOne(ctx context.Context, hosts *haproxy.Hosts, host loadbalancer.Host,
	object *networkv1alpha1.LoadBalancer, lb *loadbalancer.LoadBalancer) networkv1alpha1.HostLoadBalancerStatus {
	ctx, cancel := context.WithTimeout(ctx, writeTimeout)
	defer cancel()
	report := networkv1alpha1.HostLoadBalancerStatus{Namespace: object.Namespace, Name: object.Name, UID: string(object.UID)}
	facts, err := hosts.Observe(ctx, host, lb)
	if err != nil {
		report.Reload, report.Message = reloadNames[loadbalancer.ReloadFailed], "无法观察本机代理："+err.Error()
		return report
	}
	report.Present, report.Reload, report.Message = facts.Present, reloadNames[facts.Reload], facts.Message
	if facts.Matches {
		report.Digest = lb.Snapshot().Digest()
	}
	for _, s := range facts.Servers {
		report.Servers = append(report.Servers, networkv1alpha1.HostServerStatus{
			Port: s.Port, Address: s.Endpoint.Address, EndpointPort: s.Endpoint.Port,
			State: s.State, CheckStatus: s.CheckStatus, CheckCode: s.CheckCode,
		})
	}
	// 宣告由本机另一个循环判断，这里只用 PlanHost 决定写入。
	var write error
	switch lb.PlanHost(host.Name, facts, loadbalancer.AgentReport{Reported: true}, nil, !object.DeletionTimestamp.IsZero()) {
	case loadbalancer.ApplyConfig:
		write = hosts.Apply(ctx, host, lb)
	case loadbalancer.RemoveConfig:
		write = hosts.Remove(ctx, host, lb)
	}
	if write != nil {
		report.Message = fmt.Sprintf("写入失败：%v", write)
	}
	return report
}

func signal(c chan struct{}) {
	select {
	case c <- struct{}{}:
	default:
	}
}
