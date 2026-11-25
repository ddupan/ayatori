package imds

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"slices"
	"time"
)

// pollInterval 是重新读取本地状态和端口的间隔，也是实例启动后元数据可用的最长延迟。
// ponytail: 轮询；需要更快时订阅 netlink 的链路事件。
const pollInterval = 2 * time.Second

// reapplyInterval 是没有变化时重新下发规则的间隔：别的组件（例如 nftables.service 的 flush ruleset）
// 清掉规则后，最迟在这个时间内恢复。
const reapplyInterval = time.Minute

// slot 是一个端口的槽位：mark、路由表与监听端口都由槽位号决定。槽位在端口存在期间不变，
// 其他实例增删不会打断它的连接。
type slot struct {
	n       int
	handler *handler
	server  *http.Server
}

// Service 是 ayatori-imds。
type Service struct {
	StateDir string
	Logger   *slog.Logger

	slots     map[string]*slot
	applied   []binding
	appliedAt time.Time
}

// Run 配置地址后按间隔收敛，直到 ctx 结束。退出时保留规则：去掉拦截后，实例的请求会经网关发到
// 网络上的任意主机；监听端口关闭后请求只会失败。
func (s *Service) Run(ctx context.Context) error {
	if err := setupAddress(); err != nil {
		return err
	}
	s.slots = map[string]*slot{}
	defer func() {
		for _, sl := range s.slots {
			_ = sl.server.Close()
		}
	}()
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()
	for {
		if err := s.sync(); err != nil {
			s.Logger.Error("sync", "error", err)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

func (s *Service) sync() error {
	ports, err := readPorts(s.StateDir)
	if err != nil {
		return err
	}
	seen := map[string]bool{}
	var errs []error
	var bs []binding
	for _, p := range ports {
		seen[p.Name] = true
		sl, ok := s.slots[p.Name]
		if !ok {
			if sl, err = s.open(p); err != nil {
				errs = append(errs, err)
				continue
			}
		}
		sl.handler.instance.Store(&p.Instance)
		bridge, mac, err := portBridge(p.Name)
		if err != nil {
			errs = append(errs, err)
		} else if bridge != "" {
			bs = append(bs, binding{port: p.Name, bridge: bridge, mac: mac, slot: sl.n})
		}
	}
	for name, sl := range s.slots {
		if !seen[name] {
			_ = sl.server.Close()
			delete(s.slots, name)
			s.Logger.Info("released", "port", name, "slot", sl.n)
		}
	}
	slices.SortFunc(bs, func(a, b binding) int { return a.slot - b.slot })
	if !slices.Equal(bs, s.applied) || time.Since(s.appliedAt) > reapplyInterval {
		if err := applyRules(renderRules(bs)); err != nil {
			return errors.Join(append(errs, err)...)
		}
		if err := applyRoutes(bs); err != nil {
			return errors.Join(append(errs, err)...)
		}
		if !slices.Equal(bs, s.applied) {
			s.Logger.Info("applied", "ports", len(bs))
		}
		s.applied, s.appliedAt = bs, time.Now()
	}
	return errors.Join(errs...)
}

// open 为新端口分配最小的空闲槽位并开始监听。
func (s *Service) open(p Port) (*slot, error) {
	used := map[int]bool{}
	for _, sl := range s.slots {
		used[sl.n] = true
	}
	n := 0
	for used[n] {
		n++
	}
	l, err := listen(listenBase + n)
	if err != nil {
		return nil, err
	}
	h := newHandler(p.Instance)
	sl := &slot{n: n, handler: h, server: &http.Server{Handler: h, ReadHeaderTimeout: 5 * time.Second}}
	go func() { _ = sl.server.Serve(l) }()
	s.slots[p.Name] = sl
	s.Logger.Info("listening", "port", p.Name, "instance", p.Instance.Namespace+"/"+p.Instance.Name, "slot", n)
	return sl, nil
}
