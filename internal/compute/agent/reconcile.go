package agent

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"

	computev1alpha1 "go.ddupan.top/ayatori/api/compute/v1alpha1"
	"go.ddupan.top/ayatori/internal/compute/adapter/libvirt"
	"go.ddupan.top/ayatori/internal/compute/agent/state"
)

// observation 是一轮 reconcile 后要回写的 status：network/pool 只写本节点那一项，volume/domain 写整个 status。
type observation struct {
	kind, namespace, name string
	status                any
}

// reconcile 按本地状态收敛本机 libvirt。network、pool、volume 依次先于 domain 下发：后者引用前者；
// 删除按相反的顺序，先清理引用方。
func (a *Agent) reconcile() {
	connected := a.connect()
	a.metrics.libvirtUp.Set(boolGauge(connected))
	if !connected {
		a.metrics.domains.snapshot.Store(&[]instanceStats{})
		return
	}
	for _, kind := range slices.Backward(kinds) {
		for _, r := range a.store.List(kind) {
			if r.Deleted {
				if err := a.remove(r); err != nil {
					a.logger.Error("remove from libvirt", "kind", kind, "key", r.Key, "error", err)
				}
			}
		}
	}
	observed := map[string]observation{}
	seen := map[objectID]bool{}
	for _, kind := range kinds {
		for _, r := range a.store.List(kind) {
			if r.Deleted {
				continue
			}
			obs, err := a.apply(r)
			if err != nil {
				a.logger.Error("apply to libvirt", "kind", kind, "key", r.Key, "error", err)
			}
			if obs != nil {
				observed[kind+"/"+r.Key] = *obs
				id := objectID{obs.kind, obs.namespace, obs.name}
				seen[id] = true
				if err == nil {
					a.succeeded[id] = time.Now()
				}
			}
		}
	}
	a.observed = observed
	a.metrics.observe(a.succeeded, seen, a.seen)
	a.seen = seen
	a.collectDomainStats(seen)
	facts := a.nodeFacts()
	a.facts.Store(&facts)
}

// collectDomainStats 读取本 Agent 管理的运行中实例的资源用量，替换指标快照。读取失败时清空快照：
// 宁可缺数据，也不导出停滞的累计量。
func (a *Agent) collectDomainStats(seen map[objectID]bool) {
	ours := map[string]objectID{}
	for id := range seen {
		if id.kind == kindDomain {
			ours[domainName(id.namespace, id.name)] = id
		}
	}
	snapshot := []instanceStats{}
	stats, err := a.hv.AllDomainStats()
	if err != nil {
		a.logger.Error("read domain stats", "error", err)
	}
	for _, s := range stats {
		if id, ok := ours[s.Name]; ok {
			snapshot = append(snapshot, instanceStats{id.namespace, id.name, s})
		}
	}
	a.metrics.domains.snapshot.Store(&snapshot)
}

// connect 在没有连接或连接已断开时重连 libvirt。
func (a *Agent) connect() bool {
	if a.hv != nil {
		if a.hv.Alive() {
			return true
		}
		_ = a.hv.Close()
		a.hv = nil
	}
	hv, err := libvirt.Dial(a.config.LibvirtURI)
	if err != nil {
		a.logger.Error("connect libvirt", "uri", a.config.LibvirtURI, "error", err)
		a.connected = false
		a.facts.Store(&nodeFacts{cpuLabels: a.facts.Load().cpuLabels})
		return false
	}
	a.hv, a.connected = hv, true
	return true
}

func (a *Agent) remove(r state.Record) error {
	var err error
	switch r.Kind {
	case kindNetwork:
		err = a.hv.RemoveNetwork(r.Key)
	case kindPool:
		// pool 上还有本节点的卷或镜像对象（包括等实例清理后才删的）时先不 undefine：否则之后无法删除，
		// 文件会残留。
		for _, i := range a.store.List(kindImage) {
			var img computev1alpha1.Image
			if err := json.Unmarshal(i.Object, &img); err != nil {
				return err
			}
			if img.Spec.Pool == r.Key {
				a.logger.Info("pool still has images, removing later", "key", r.Key, "image", i.Key)
				return nil
			}
		}
		for _, v := range a.store.List(kindVolume) {
			var vol computev1alpha1.LibvirtStorageVolume
			if err := json.Unmarshal(v.Object, &vol); err != nil {
				return err
			}
			if vol.Spec.Libvirt.Pool == r.Key {
				a.logger.Info("pool still has volumes, removing later", "key", r.Key, "volume", v.Key)
				return nil
			}
		}
		err = a.hv.RemovePool(r.Key)
	case kindVolume:
		var removed bool
		if removed, err = a.removeVolume(r); err == nil && !removed {
			return nil
		}
	case kindImage:
		var removed bool
		if removed, err = a.removeImage(r); err == nil && !removed {
			return nil
		}
	case kindDomain:
		ns, name, _ := strings.Cut(r.Key, "/")
		err = a.hv.RemoveDomain(domainName(ns, name))
	}
	if err != nil {
		return err
	}
	a.logger.Info("removed", "kind", r.Kind, "key", r.Key)
	return a.store.Forget(r.Kind, r.Key)
}

func (a *Agent) apply(r state.Record) (*observation, error) {
	switch r.Kind {
	case kindNetwork:
		var n computev1alpha1.LibvirtNetwork
		if err := json.Unmarshal(r.Object, &n); err != nil {
			return nil, err
		}
		err := a.applyNetwork(r, &n)
		return a.nodeObservation(kindNetwork, n.Name, n.Generation, err), err
	case kindPool:
		var p computev1alpha1.LibvirtStoragePool
		if err := json.Unmarshal(r.Object, &p); err != nil {
			return nil, err
		}
		err := a.applyPool(r, &p)
		return a.nodeObservation(kindPool, p.Name, p.Generation, err), err
	case kindVolume:
		var v computev1alpha1.LibvirtStorageVolume
		if err := json.Unmarshal(r.Object, &v); err != nil {
			return nil, err
		}
		status, err := a.applyVolume(r, &v)
		return &observation{kind: kindVolume, name: v.Name, status: status}, err
	case kindImage:
		var img computev1alpha1.Image
		if err := json.Unmarshal(r.Object, &img); err != nil {
			return nil, err
		}
		status, err := a.applyImage(r, &img)
		return &observation{kind: kindImage, name: img.Name, status: status}, err
	case kindDomain:
		var d computev1alpha1.LibvirtDomain
		if err := json.Unmarshal(r.Object, &d); err != nil {
			return nil, err
		}
		status, err := a.applyDomain(r, &d)
		return &observation{kind: kindDomain, namespace: d.Namespace, name: d.Name, status: status}, err
	}
	return nil, nil
}

// applyNetwork 与 applyPool：spec 变了，或读回的配置与上次下发的不同（有人带外修改过），就重新定义；
// 然后确保处于活动状态。
func (a *Agent) applyNetwork(r state.Record, n *computev1alpha1.LibvirtNetwork) error {
	want, err := prepareNetwork(n)
	if err != nil {
		return err
	}
	read := func() (libvirt.Object, error) { return a.hv.Network(n.Name) }
	if err := a.define(r, want, read, a.hv.DefineNetwork, plainHash); err != nil {
		return err
	}
	return a.hv.StartNetwork(n.Name)
}

func (a *Agent) applyPool(r state.Record, p *computev1alpha1.LibvirtStoragePool) error {
	want, err := preparePool(p)
	if err != nil {
		return err
	}
	read := func() (libvirt.Object, error) { return a.hv.Pool(p.Name) }
	if err := a.define(r, want, read, a.hv.DefinePool, poolHash); err != nil {
		return err
	}
	if err := a.hv.StartPool(p.Name); err != nil {
		return err
	}
	capacity, available, err := a.hv.PoolSpace(p.Name)
	if err != nil {
		return err
	}
	a.metrics.poolBytes.WithLabelValues(p.Name, "capacity").Set(float64(capacity))
	a.metrics.poolBytes.WithLabelValues(p.Name, "available").Set(float64(available))
	return nil
}

// applyVolume 在卷不存在时按 XML 创建，声明了 source.image 时从镜像复制；已存在就接管，不修改也不
// 覆盖：卷的内容是数据。
func (a *Agent) applyVolume(r state.Record, v *computev1alpha1.LibvirtStorageVolume) (computev1alpha1.LibvirtStorageVolumeStatus, error) {
	status := computev1alpha1.LibvirtStorageVolumeStatus{ObservedGeneration: v.Generation}
	fail := func(err error) (computev1alpha1.LibvirtStorageVolumeStatus, error) {
		status.Message = err.Error()
		return status, err
	}
	cur, err := a.hv.Volume(v.Spec.Libvirt.Pool, v.Name)
	if err != nil {
		return fail(err)
	}
	sha := r.AppliedInputHash
	if !cur.Found {
		want, err := prepareVolume(v)
		if err != nil {
			return fail(err)
		}
		if v.Spec.Source != nil {
			sha, err = a.createVolumeFromImage(r, v, want)
		} else {
			err = a.hv.CreateVolume(v.Spec.Libvirt.Pool, want)
		}
		if err != nil {
			return fail(err)
		}
		a.logger.Info("created", "kind", kindVolume, "key", v.Name, "pool", v.Spec.Libvirt.Pool)
		if cur, err = a.hv.Volume(v.Spec.Libvirt.Pool, v.Name); err != nil {
			return fail(err)
		}
	}
	status.Path, status.Capacity = cur.Path, int64(cur.Capacity)
	if v.Spec.Source != nil {
		status.Image, status.SHA256 = v.Spec.Source.Image, sha
	}
	return status, nil
}

// removeVolume 按 reclaimPolicy 处理已删除的卷对象。Delete 时，本节点上仍有实例（包括待清理的）引用它
// 就先不删，等实例清理后的下一轮：与 PVC 保护相同，避免删掉正在使用的磁盘。
func (a *Agent) removeVolume(r state.Record) (bool, error) {
	var v computev1alpha1.LibvirtStorageVolume
	if err := json.Unmarshal(r.Object, &v); err != nil {
		return false, err
	}
	if v.Spec.ReclaimPolicy != computev1alpha1.ReclaimDelete {
		return true, nil
	}
	for _, d := range a.store.List(kindDomain) {
		var dom computev1alpha1.LibvirtDomain
		if err := json.Unmarshal(d.Object, &dom); err != nil {
			return false, err
		}
		for _, disk := range dom.Spec.Disks {
			if disk.Volume == v.Name {
				a.logger.Info("volume still referenced, deleting later", "key", r.Key, "domain", d.Key)
				return false, nil
			}
		}
	}
	return true, a.hv.DeleteVolume(v.Spec.Libvirt.Pool, v.Name)
}

// define 在需要时下发 want，并记录输入与读回配置的 hash。read 返回对象的现状（持久配置）。
func (a *Agent) define(r state.Record, want string, read func() (libvirt.Object, error), define func(string) error,
	normalize func(string) (string, error)) error {
	cur, err := read()
	if err != nil {
		return err
	}
	in := hash(want)
	if cur.Found && in == r.AppliedInputHash {
		h, err := normalize(cur.XML)
		if err != nil {
			return err
		}
		if h == r.AppliedHash {
			return nil
		}
		a.logger.Info("configuration changed outside Ayatori, redefining", "kind", r.Kind, "key", r.Key)
	}
	if err := define(want); err != nil {
		return err
	}
	if cur, err = read(); err != nil {
		return err
	}
	h, err := normalize(cur.XML)
	if err != nil {
		return err
	}
	a.logger.Info("defined", "kind", r.Kind, "key", r.Key)
	return a.store.Update(r.Kind, r.Key, func(rec *state.Record) { rec.AppliedInputHash, rec.AppliedHash = in, h })
}

func (a *Agent) nodeObservation(kind, name string, generation int64, err error) *observation {
	s := computev1alpha1.NodeObjectStatus{NodeName: a.config.NodeName, ObservedGeneration: generation, Active: err == nil}
	if err != nil {
		s.Message = err.Error()
	}
	return &observation{kind: kind, name: name, status: s}
}

// resolveVolume 按本地状态中的卷对象找到它的 pool，再从 libvirt 取路径与格式。卷对象按 nodeName 分配，
// 只有本节点的卷会出现在本地状态中；待删除的卷仍可解析，引用它的实例清理后才会删除。
func (a *Agent) resolveVolume(name string) (Volume, error) {
	for _, r := range a.store.List(kindVolume) {
		if r.Key != name {
			continue
		}
		var v computev1alpha1.LibvirtStorageVolume
		if err := json.Unmarshal(r.Object, &v); err != nil {
			return Volume{}, err
		}
		cur, err := a.hv.Volume(v.Spec.Libvirt.Pool, name)
		if err == nil && !cur.Found {
			err = errors.New("volume not created yet")
		}
		return Volume{Path: cur.Path, Block: cur.Block, Format: cur.Format}, err
	}
	return Volume{}, errors.New("no such volume on this node")
}

// applyDomain 下发实例并按 runState 与观察到的电源意图决定是否启动：
// 客户机内关机或 destroy 是有意的停止，之后（包括宿主机重启后）不再自动拉起；崩溃与首次定义后拉起。
func (a *Agent) applyDomain(r state.Record, d *computev1alpha1.LibvirtDomain) (computev1alpha1.LibvirtDomainStatus, error) {
	status := computev1alpha1.LibvirtDomainStatus{ObservedGeneration: d.Generation, NodeName: a.config.NodeName}
	fail := func(err error) (computev1alpha1.LibvirtDomainStatus, error) {
		status.Message = err.Error()
		return status, err
	}
	want, err := prepareDomain(d, a.resolveDisk)
	if err != nil {
		return fail(err)
	}
	name := domainName(d.Namespace, d.Name)
	running := a.metrics.running.WithLabelValues(d.Namespace, d.Name)
	shouldRun := func(poweredOff bool) {
		a.metrics.shouldRun.WithLabelValues(d.Namespace, d.Name).Set(boolGauge(d.Spec.RunState != computev1alpha1.RunStateStopped && !poweredOff))
	}
	shouldRun(r.PoweredOff)
	read := func() (libvirt.Object, error) {
		dom, err := a.hv.Domain(name)
		return libvirt.Object{Found: dom.Found, XML: dom.XML}, err
	}
	if err := a.define(r, want, read, a.hv.DefineDomain, plainHash); err != nil {
		return fail(err)
	}
	cur, err := a.hv.Domain(name)
	if err != nil {
		return fail(err)
	}
	running.Set(boolGauge(cur.Running()))
	poweredOff := r.PoweredOff
	switch {
	case cur.Running():
		poweredOff = false
	case cur.Reason == "shutdown" || cur.Reason == "destroyed":
		poweredOff = true
	case d.Spec.RunState != computev1alpha1.RunStateStopped && !poweredOff:
		if err := a.hv.StartDomain(name); err != nil {
			status.State, status.Reason = cur.State, cur.Reason
			return fail(err)
		}
		a.logger.Info("started", "domain", name, "previous", cur.State+"/"+cur.Reason)
		if cur, err = a.hv.Domain(name); err != nil {
			return fail(err)
		}
		running.Set(boolGauge(cur.Running()))
	}
	if err := a.store.Update(r.Kind, r.Key, func(rec *state.Record) { rec.PoweredOff = poweredOff }); err != nil {
		return fail(err)
	}
	shouldRun(poweredOff)
	status.State, status.Reason = cur.State, cur.Reason
	return status, nil
}

// report 把与上次写入不同的观察写回 API；失败的留到下一轮，控制面不可用时只是暂时写不进去。
func (a *Agent) report(ctx context.Context) {
	kube := a.kube.Load()
	if kube == nil {
		return
	}
	for id, obs := range a.observed {
		if prev, ok := a.reported[id]; ok && reflect.DeepEqual(prev, obs) {
			continue
		}
		if err := a.writeStatus(ctx, *kube, obs); err != nil {
			a.logger.Debug("write status", "object", id, "error", err)
			continue
		}
		a.reported[id] = obs
	}
	for id := range a.reported {
		if _, ok := a.observed[id]; !ok {
			delete(a.reported, id)
		}
	}
}

// writeStatus 以本节点专属的 field manager 做 server-side apply：network/pool 的 status.nodes 是以
// nodeName 为键的列表，各节点只拥有自己那一项。
func (a *Agent) writeStatus(ctx context.Context, kube client.Client, obs observation) error {
	u := &unstructured.Unstructured{}
	u.SetAPIVersion(computev1alpha1.GroupVersion.String())
	u.SetKind(obs.kind)
	u.SetNamespace(obs.namespace)
	u.SetName(obs.name)
	data, err := json.Marshal(obs.status)
	if err != nil {
		return err
	}
	var status map[string]any
	if err := json.Unmarshal(data, &status); err != nil {
		return err
	}
	if obs.kind == kindNetwork || obs.kind == kindPool || obs.kind == kindImage {
		u.Object["status"] = map[string]any{"nodes": []any{status}}
	} else {
		u.Object["status"] = status
	}
	err = kube.Status().Apply(ctx, client.ApplyConfigurationFromUnstructured(u),
		client.FieldOwner("ayatori-compute-agent/"+a.config.NodeName), client.ForceOwnership)
	if apierrors.IsNotFound(err) {
		return nil // 对象已被删除，sync 会标记删除
	}
	return err
}
