// Package haproxy 经 HAProxy Data Plane API v3 管理 LB 主机上的 frontend/backend，只返回事实。
//
// 部署契约（由 homelab-infra 保证，adapter 不检查）：
//   - Ayatori 是 haproxy.cfg 中 lb_ 前缀 section 的唯一写入方；
//   - net.ipv4.ip_nonlocal_bind=1，使 frontend 可在宣告前绑定 VIP；
//   - reload 前以 server-state-file 保存 server 状态，避免 reload 后全部 server 回到初始状态；
//   - hard-stop-after 限定旧 worker 排空已有连接的时长；
//   - 使用 DNS endpoint 时，配置名为 Hosts.Resolvers 的 resolvers section。
package haproxy

//go:generate make -C ../../../.. generate-dataplaneapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/netip"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"time"

	"git.ddupan.top/panxiao81/ayatori/internal/loadbalancer/adapter/haproxy/dataplaneapi"
	"git.ddupan.top/panxiao81/ayatori/internal/loadbalancer/domain/loadbalancer"
)

// Hosts 对任意 Host 执行同一套操作；HTTP 客户端负责 mTLS。
type Hosts struct {
	HTTP     *http.Client
	Username string
	Password string
	// ConnectTimeout 是连接后端的超时，平台统一配置。
	ConnectTimeout time.Duration
	// Resolvers 是 DNS endpoint 使用的 resolvers section 名称；为空时只在启动时解析。
	Resolvers string
	// Now 只供测试替换。
	Now func() time.Time

	drains drains
}

func (h *Hosts) now() time.Time {
	if h.Now == nil {
		return time.Now()
	}
	return h.Now()
}

// Observe 读取该 LoadBalancer 在 Host 上的配置，并与期望比较。
func (h *Hosts) Observe(ctx context.Context, host loadbalancer.Host, lb *loadbalancer.LoadBalancer) (loadbalancer.ProxyFacts, error) {
	uid, vip, ports := desired(lb)
	api, err := h.client(host)
	if err != nil {
		return loadbalancer.ProxyFacts{}, err
	}
	frontends, backends, err := sections(ctx, api, nil)
	if err != nil {
		return loadbalancer.ProxyFacts{}, err
	}
	wantFrontends, wantBackends, _ := h.translate(uid, vip, ports)
	gotFrontends, gotBackends := owned(frontends, uid), owned(backends, uid)
	facts := loadbalancer.ProxyFacts{
		Present: len(gotFrontends)+len(gotBackends) > 0,
		Matches: reflect.DeepEqual(gotFrontends, wantFrontends) && reflect.DeepEqual(gotBackends, wantBackends),
	}
	if facts.Reload, facts.Message, err = lastReload(ctx, api); err != nil {
		return loadbalancer.ProxyFacts{}, err
	}
	if facts.Matches && facts.Reload == loadbalancer.ReloadSucceeded {
		var loaded bool
		if facts.Matches, loaded, err = runtimeReady(ctx, api, wantBackends); err != nil {
			return loadbalancer.ProxyFacts{}, err
		}
		if !loaded {
			// 配置已写入但运行中的进程尚未加载：reload 已排期未执行。
			facts.Matches, facts.Reload = true, loadbalancer.ReloadPending
		}
	}
	return facts, nil
}

// Apply 在一个事务中写入该 LoadBalancer 的全部 section 并删除多余端口，提交后由代理 reload。
// 他人在提交前写入时提交返回 409，由下一轮重新观察。删除时 Remove 以空期望走同一路径。
func (h *Hosts) Apply(ctx context.Context, host loadbalancer.Host, lb *loadbalancer.LoadBalancer) error {
	uid, vip, ports := desired(lb)
	return h.apply(ctx, host, uid, vip, ports)
}

// Remove 在一个事务中删除该 LoadBalancer 在 Host 上的全部 section。
func (h *Hosts) Remove(ctx context.Context, host loadbalancer.Host, lb *loadbalancer.LoadBalancer) error {
	return h.apply(ctx, host, lb.Snapshot().Owner.UID, "", nil)
}

func desired(lb *loadbalancer.LoadBalancer) (uid, vip string, ports []loadbalancer.Port) {
	snapshot := lb.Snapshot()
	return snapshot.Owner.UID, snapshot.Address.String(), snapshot.Ports
}

func (h *Hosts) apply(ctx context.Context, host loadbalancer.Host, uid, vip string, ports []loadbalancer.Port) error {
	api, err := h.client(host)
	if err != nil {
		return err
	}
	wantFrontends, wantBackends, drainTimeouts := h.translate(uid, vip, ports)
	frontends, backends, err := sections(ctx, api, nil)
	if err != nil {
		return err
	}
	if gotBackends := owned(backends, uid); sameStructure(owned(frontends, uid), wantFrontends, gotBackends, wantBackends) {
		return h.reconcileServers(ctx, api, host.Name, gotBackends, wantBackends, drainTimeouts)
	}
	// 新建、端口增减或检查、超时等 section 级变化需要 reload：整段替换。
	return transact(ctx, api, func(tx *string, frontends map[string]dataplaneapi.Frontend, backends map[string]dataplaneapi.Backend) error {
		full := new(true)
		for name, b := range wantBackends {
			if err := upsert(name, backends, func() (response, error) {
				return api.CreateBackendWithResponse(ctx, &dataplaneapi.CreateBackendParams{TransactionId: tx, FullSection: full}, b)
			}, func() (response, error) {
				return api.ReplaceBackendWithResponse(ctx, name, &dataplaneapi.ReplaceBackendParams{TransactionId: tx, FullSection: full}, b)
			}); err != nil {
				return err
			}
		}
		for name, f := range wantFrontends {
			if err := upsert(name, frontends, func() (response, error) {
				return api.CreateFrontendWithResponse(ctx, &dataplaneapi.CreateFrontendParams{TransactionId: tx, FullSection: full}, f)
			}, func() (response, error) {
				return api.ReplaceFrontendWithResponse(ctx, name, &dataplaneapi.ReplaceFrontendParams{TransactionId: tx, FullSection: full}, f)
			}); err != nil {
				return err
			}
		}
		// 先删 frontend 再删其 backend，避免中间状态引用不存在的 backend。
		for name := range owned(frontends, uid) {
			if _, keep := wantFrontends[name]; !keep {
				if err := check(api.DeleteFrontendWithResponse(ctx, name, &dataplaneapi.DeleteFrontendParams{TransactionId: tx})); err != nil {
					return err
				}
			}
		}
		for name := range owned(backends, uid) {
			if _, keep := wantBackends[name]; !keep {
				if err := check(api.DeleteBackendWithResponse(ctx, name, &dataplaneapi.DeleteBackendParams{TransactionId: tx})); err != nil {
					return err
				}
			}
		}
		return nil
	})
}

func (h *Hosts) client(host loadbalancer.Host) (*dataplaneapi.ClientWithResponses, error) {
	return dataplaneapi.NewClientWithResponses(strings.TrimSuffix(host.ProxyURL, "/")+"/v3",
		dataplaneapi.WithHTTPClient(h.HTTP),
		dataplaneapi.WithRequestEditorFn(func(_ context.Context, request *http.Request) error {
			request.SetBasicAuth(h.Username, h.Password)
			return nil
		}))
}

// translate 把期望配置翻译为 section。没有健康 endpoint 时 HAProxy 拒绝连接（fail-closed）；
// server 以 fully-down 起始，必须连续通过检查才接收流量，被判失败时断开其已有连接。
func (h *Hosts) translate(uid, vip string, ports []loadbalancer.Port) (map[string]dataplaneapi.Frontend, map[string]dataplaneapi.Backend, map[string]time.Duration) {
	frontends := map[string]dataplaneapi.Frontend{}
	backends := map[string]dataplaneapi.Backend{}
	drainTimeouts := map[string]time.Duration{}
	for _, port := range ports {
		// HAProxy 3.3 起 frontend 与 backend 不能同名。
		name := sectionPrefix(uid) + strconv.Itoa(int(port.Port))
		backendName := name + "_backend"
		check := port.HealthCheck
		backend := dataplaneapi.Backend{
			Name:           backendName,
			Mode:           new(dataplaneapi.BackendModeTcp),
			Balance:        &dataplaneapi.Balance{Algorithm: dataplaneapi.BalanceAlgorithmLeastconn},
			ServerTimeout:  millis(port.IdleTimeout),
			ConnectTimeout: millis(h.ConnectTimeout),
			CheckTimeout:   millis(check.Timeout),
		}
		if check.Path != "" {
			backend.AdvCheck = new(dataplaneapi.BackendAdvCheckHttpchk)
			backend.HttpchkParams = &dataplaneapi.HttpchkParams{Method: new(dataplaneapi.HttpchkParamsMethodGET), Uri: new(check.Path)}
			backend.HttpCheckList = &dataplaneapi.HttpChecks{{
				Type: dataplaneapi.HttpCheckTypeExpect, Match: new(dataplaneapi.HttpCheckMatchRstatus), Pattern: new(statusPattern(check.ExpectedStatuses)),
			}}
		}
		if len(port.Endpoints) > 0 {
			servers := make(map[string]dataplaneapi.Server, len(port.Endpoints))
			for _, endpoint := range port.Endpoints {
				server := h.server(endpoint, check, port.ProxyProtocol)
				servers[server.Name] = server
			}
			backend.Servers = &servers
		}
		backends[backendName] = backend
		drainTimeouts[backendName] = port.DrainTimeout
		frontends[name] = dataplaneapi.Frontend{
			Name:           name,
			Mode:           new(dataplaneapi.FrontendModeTcp),
			ClientTimeout:  millis(port.IdleTimeout),
			DefaultBackend: new(backendName),
			Binds:          &map[string]dataplaneapi.Bind{"vip": {Name: "vip", Address: new(vip), Port: new(int(port.Port))}},
		}
	}
	return frontends, backends, drainTimeouts
}

func (h *Hosts) server(endpoint loadbalancer.Endpoint, check loadbalancer.HealthCheck, proxy loadbalancer.ProxyProtocol) dataplaneapi.Server {
	server := dataplaneapi.Server{
		Name:         endpoint.Address + ":" + strconv.Itoa(int(endpoint.Port)),
		Address:      endpoint.Address,
		Port:         new(int(endpoint.Port)),
		Check:        new(dataplaneapi.ServerCheckEnabled),
		Inter:        millis(check.Interval),
		Rise:         new(int(check.HealthyThreshold)),
		Fall:         new(int(check.UnhealthyThreshold)),
		InitState:    new(dataplaneapi.ServerInitStateFullyDown),
		OnMarkedDown: new(dataplaneapi.ServerOnMarkedDownShutdownSessions),
	}
	if check.Port != 0 {
		server.HealthCheckPort = new(int(check.Port))
	}
	switch proxy {
	case loadbalancer.ProxyProtocolV1:
		server.SendProxy, server.CheckSendProxy = new(dataplaneapi.ServerSendProxyEnabled), new(dataplaneapi.ServerCheckSendProxyEnabled)
	case loadbalancer.ProxyProtocolV2:
		server.SendProxyV2, server.CheckSendProxy = new(dataplaneapi.ServerSendProxyV2Enabled), new(dataplaneapi.ServerCheckSendProxyEnabled)
	}
	if _, err := netip.ParseAddr(endpoint.Address); err != nil {
		// DNS 名称：启动时无法解析也不阻止加载，由 resolvers 周期解析。
		server.InitAddr = new("none")
		if h.Resolvers != "" {
			server.Resolvers = new(h.Resolvers)
		}
	}
	return server
}

func statusPattern(statuses []int32) string {
	if len(statuses) == 0 {
		statuses = []int32{200}
	}
	codes := make([]string, 0, len(statuses))
	for _, status := range slices.Sorted(slices.Values(statuses)) {
		codes = append(codes, strconv.Itoa(int(status)))
	}
	return "^(" + strings.Join(codes, "|") + ")$"
}

func transact(ctx context.Context, api *dataplaneapi.ClientWithResponses,
	apply func(tx *string, frontends map[string]dataplaneapi.Frontend, backends map[string]dataplaneapi.Backend) error) error {
	version, err := api.GetConfigurationVersionWithResponse(ctx, &dataplaneapi.GetConfigurationVersionParams{})
	if err := check(version, err); err != nil {
		return err
	}
	started, err := api.StartTransactionWithResponse(ctx, &dataplaneapi.StartTransactionParams{Version: *version.JSON200})
	if err := check(started, err); err != nil {
		return err
	}
	tx := started.JSON201.Id
	frontends, backends, err := sections(ctx, api, tx)
	if err == nil {
		err = apply(tx, frontends, backends)
	}
	if err == nil {
		return check(api.CommitTransactionWithResponse(ctx, *tx, &dataplaneapi.CommitTransactionParams{}))
	}
	// 未提交的事务不会生效，但占用代理的事务上限，需要清理。
	return errors.Join(err, check(api.DeleteTransactionWithResponse(context.WithoutCancel(ctx), *tx)))
}

func sections(ctx context.Context, api *dataplaneapi.ClientWithResponses, tx *string) (map[string]dataplaneapi.Frontend, map[string]dataplaneapi.Backend, error) {
	frontends, err := api.GetFrontendsWithResponse(ctx, &dataplaneapi.GetFrontendsParams{TransactionId: tx, FullSection: new(true)})
	if err := check(frontends, err); err != nil {
		return nil, nil, err
	}
	backends, err := api.GetBackendsWithResponse(ctx, &dataplaneapi.GetBackendsParams{TransactionId: tx, FullSection: new(true)})
	if err := check(backends, err); err != nil {
		return nil, nil, err
	}
	return byName(*frontends.JSON200, func(f dataplaneapi.Frontend) string { return f.Name }),
		byName(*backends.JSON200, func(b dataplaneapi.Backend) string { return b.Name }), nil
}

// lastReload 返回 Host 最近一次 reload 的结果；仍有 reload 进行中时为 Pending。
func lastReload(ctx context.Context, api *dataplaneapi.ClientWithResponses) (loadbalancer.Reload, string, error) {
	response, err := api.GetReloadsWithResponse(ctx)
	if err := check(response, err); err != nil {
		return 0, "", err
	}
	var latest *dataplaneapi.Reload
	for i, reload := range *response.JSON200 {
		if reload.Status != nil && *reload.Status == dataplaneapi.ReloadStatusInProgress {
			return loadbalancer.ReloadPending, "", nil
		}
		if latest == nil || value(reload.ReloadTimestamp) > value(latest.ReloadTimestamp) {
			latest = &(*response.JSON200)[i]
		}
	}
	if latest != nil && latest.Status != nil && *latest.Status == dataplaneapi.ReloadStatusFailed {
		return loadbalancer.ReloadFailed, value(latest.Response), nil
	}
	return loadbalancer.ReloadSucceeded, "", nil
}

type response interface{ StatusCode() int }

// check 把非 2xx 响应转为带响应正文的错误；正文来自代理，不含凭据。
func check(r response, err error) error {
	if err != nil {
		return err
	}
	if code := r.StatusCode(); code >= 300 {
		body := reflect.ValueOf(r).Elem().FieldByName("Body").Bytes()
		return fmt.Errorf("data plane API: HTTP %d: %s", code, strings.TrimSpace(string(body)))
	}
	return nil
}

func upsert[T any](name string, existing map[string]T, create, replace func() (response, error)) error {
	if _, ok := existing[name]; ok {
		return check(replace())
	}
	return check(create())
}

func sectionPrefix(uid string) string { return "lb_" + uid + "_" }

// owned 只保留属于该 LoadBalancer 的 section；其他 section 不观察也不修改。
func owned[T any](sections map[string]T, uid string) map[string]T {
	result := map[string]T{}
	for name, section := range sections {
		if strings.HasPrefix(name, sectionPrefix(uid)) {
			result[name] = section
		}
	}
	return result
}

func byName[T any](items []T, name func(T) string) map[string]T {
	result := make(map[string]T, len(items))
	for _, item := range items {
		result[name(item)] = item
	}
	return result
}

func millis(d time.Duration) *int { return new(int(d.Milliseconds())) }

//go:fix inline

func value[T any](p *T) T {
	var zero T
	if p == nil {
		return zero
	}
	return *p
}
