package haproxy

import (
	"context"
	"net/http"
	"net/netip"
	"reflect"
	"sync"
	"time"

	"git.ddupan.top/panxiao81/ayatori/internal/loadbalancer/adapter/haproxy/dataplaneapi"
)

// drains 记录 server 开始排空的时刻。HAProxy 的 lastchg 只随健康状态变化，不能表示排空时长。
// ponytail: 只在内存中；controller 重启后排空重新计时，只会延长、不会缩短排空。需要精确上限时
// 再把开始时刻持久化。
type drains struct {
	mu      sync.Mutex
	started map[string]time.Time
}

func (d *drains) since(key string, now time.Time) time.Duration {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.started == nil {
		d.started = map[string]time.Time{}
	}
	if _, ok := d.started[key]; !ok {
		d.started[key] = now
	}
	return now.Sub(d.started[key])
}

func (d *drains) forget(key string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.started, key)
}

// sameStructure 表示两边的 section 集合相同，且除 server 外的参数一致；此时 endpoint 变化
// 可以逐个 server 经 runtime 生效，无需 reload。
func sameStructure(gotFrontends, wantFrontends map[string]dataplaneapi.Frontend, gotBackends, wantBackends map[string]dataplaneapi.Backend) bool {
	if !reflect.DeepEqual(gotFrontends, wantFrontends) || len(gotBackends) != len(wantBackends) {
		return false
	}
	for name, want := range wantBackends {
		got, ok := gotBackends[name]
		if !ok {
			return false
		}
		got.Servers, want.Servers = nil, nil
		if !reflect.DeepEqual(got, want) {
			return false
		}
	}
	return true
}

// runtimeReady 表示期望的 server 都已在 runtime 中处于 ready；dataplaneapi 动态添加的 server
// 停在 maint，需要显式启用。loaded 为 false 表示运行中的进程尚未加载这些 backend，即 reload 未生效。
// DNS endpoint 解析失败时同样报告 maint，无法与未启用区分，因此只检查 IP endpoint。
// ponytail: DNS endpoint 启用失败只在新增时重试一次；需要时改用 show servers state 区分维护原因。
func runtimeReady(ctx context.Context, api *dataplaneapi.ClientWithResponses, backends map[string]dataplaneapi.Backend) (ready, loaded bool, err error) {
	ready = true
	for name, backend := range backends {
		states, err := adminStates(ctx, api, name)
		if err != nil {
			return false, false, err
		}
		if states == nil {
			return false, false, nil
		}
		for name, server := range value(backend.Servers) {
			if isIP(server.Address) {
				ready = ready && states[name] == dataplaneapi.Ready
			}
		}
	}
	return ready, true, nil
}

// reconcileServers 逐个 server 收敛：新增后启用；移除先 drain，会话归零或超过 drainTimeout
// 后转 maint 并删除。会话归零时删除走 runtime，不 reload；超时仍有会话时删除退回 reload，
// 剩余连接由旧 worker 服务至 hard-stop-after。
func (h *Hosts) reconcileServers(ctx context.Context, api *dataplaneapi.ClientWithResponses, host string,
	got, want map[string]dataplaneapi.Backend, drainTimeouts map[string]time.Duration) error {
	for backend, wantBackend := range want {
		states, err := adminStates(ctx, api, backend)
		if err != nil {
			return err
		}
		gotServers, wantServers := value(got[backend].Servers), value(wantBackend.Servers)
		for name, server := range wantServers {
			existing, ok := gotServers[name]
			switch {
			case !ok:
				err = configWrite(ctx, api, func(version *int) (response, error) {
					return api.CreateServerBackendWithResponse(ctx, backend, &dataplaneapi.CreateServerBackendParams{Version: version}, server)
				})
				if err == nil {
					err = setAdmin(ctx, api, backend, name, dataplaneapi.Ready)
				}
			case !reflect.DeepEqual(existing, server):
				err = configWrite(ctx, api, func(version *int) (response, error) {
					return api.ReplaceServerBackendWithResponse(ctx, backend, name, &dataplaneapi.ReplaceServerBackendParams{Version: version}, server)
				})
			case states[name] != dataplaneapi.Ready && isIP(server.Address):
				err = setAdmin(ctx, api, backend, name, dataplaneapi.Ready)
			}
			if err != nil {
				return err
			}
		}
		for name := range gotServers {
			if _, keep := wantServers[name]; keep {
				continue
			}
			if err := h.removeServer(ctx, api, host, backend, name, states[name], drainTimeouts[backend]); err != nil {
				return err
			}
		}
	}
	return nil
}

func (h *Hosts) removeServer(ctx context.Context, api *dataplaneapi.ClientWithResponses, host, backend, name string,
	state dataplaneapi.RuntimeServerAdminState, timeout time.Duration) error {
	key := host + "/" + backend + "/" + name
	if state == dataplaneapi.Ready {
		h.drains.since(key, h.now())
		return setAdmin(ctx, api, backend, name, dataplaneapi.Drain)
	}
	if state == dataplaneapi.Drain {
		sessions, err := currentSessions(ctx, api, backend, name)
		if err != nil {
			return err
		}
		if sessions > 0 && h.drains.since(key, h.now()) < timeout {
			return nil
		}
		if err := setAdmin(ctx, api, backend, name, dataplaneapi.Maint); err != nil {
			return err
		}
	}
	if err := configWrite(ctx, api, func(version *int) (response, error) {
		return api.DeleteServerBackendWithResponse(ctx, backend, name, &dataplaneapi.DeleteServerBackendParams{Version: version})
	}); err != nil {
		return err
	}
	h.drains.forget(key)
	return nil
}

func isIP(address string) bool {
	_, err := netip.ParseAddr(address)
	return err == nil
}

// configWrite 以当前配置版本写入；他人并发写入时返回 409，由下一轮重新观察。
func configWrite(ctx context.Context, api *dataplaneapi.ClientWithResponses, write func(version *int) (response, error)) error {
	version, err := api.GetConfigurationVersionWithResponse(ctx, &dataplaneapi.GetConfigurationVersionParams{})
	if err := check(version, err); err != nil {
		return err
	}
	return check(write(version.JSON200))
}

func setAdmin(ctx context.Context, api *dataplaneapi.ClientWithResponses, backend, name string, state dataplaneapi.RuntimeServerAdminState) error {
	return check(api.ReplaceRuntimeServerWithResponse(ctx, backend, name, dataplaneapi.RuntimeServer{AdminState: &state}))
}

// adminStates 返回 backend 在运行中进程里的 server 管理状态；进程尚未加载该 backend 时为 nil。
func adminStates(ctx context.Context, api *dataplaneapi.ClientWithResponses, backend string) (map[string]dataplaneapi.RuntimeServerAdminState, error) {
	response, err := api.GetAllRuntimeServerWithResponse(ctx, backend)
	if err == nil && response.StatusCode() == http.StatusNotFound {
		return nil, nil
	}
	if err := check(response, err); err != nil {
		return nil, err
	}
	states := map[string]dataplaneapi.RuntimeServerAdminState{}
	for _, server := range value(response.JSON200) {
		states[value(server.Name)] = value(server.AdminState)
	}
	return states, nil
}

func currentSessions(ctx context.Context, api *dataplaneapi.ClientWithResponses, backend, name string) (int, error) {
	response, err := api.GetStatsWithResponse(ctx, &dataplaneapi.GetStatsParams{
		Type: new(dataplaneapi.GetStatsParamsTypeServer), Parent: &backend, Name: &name,
	})
	if err := check(response, err); err != nil {
		return 0, err
	}
	for _, stat := range value(value(response.JSON200).Stats) {
		if value(stat.Name) == name && stat.Stats != nil {
			return value(stat.Stats.Scur), nil
		}
	}
	return 0, nil
}
