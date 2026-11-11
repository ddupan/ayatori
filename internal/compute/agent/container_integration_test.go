//go:build integration

package agent_test

import (
	"errors"
	"fmt"
	"maps"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"sigs.k8s.io/controller-runtime/pkg/client"

	computev1alpha1 "go.ddupan.top/ayatori/api/compute/v1alpha1"
	"go.ddupan.top/ayatori/internal/compute/adapter/libvirt"
	"go.ddupan.top/ayatori/internal/compute/adapter/lxc"
)

const (
	stateRunning = "running"
	stateStopped = "stopped"
)

// fakeContainers 代替 liblxc，跨 Agent 重启保留，就像节点上的容器。真实生命周期在 Dev 环境验证。
type fakeContainers struct {
	mu      sync.Mutex
	configs map[string]string
	states  map[string]string
	starts  map[string]int
}

var containers = &fakeContainers{configs: map[string]string{}, states: map[string]string{}, starts: map[string]int{}}

func (f *fakeContainers) Container(name string) (lxc.Container, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.configs[name]; !ok {
		return lxc.Container{}, nil
	}
	return lxc.Container{Found: true, State: f.states[name]}, nil
}

func (f *fakeContainers) Config(name string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.configs[name], nil
}

func (f *fakeContainers) Define(name, config string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.configs[name] = config
	if f.states[name] == "" {
		f.states[name] = stateStopped
	}
	return nil
}

func (f *fakeContainers) Start(name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.states[name] = stateRunning
	f.starts[name]++
	return nil
}

func (f *fakeContainers) Remove(name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.configs, name)
	delete(f.states, name)
	return nil
}

func (f *fakeContainers) GrowRootfs(string, string, uint64) error { return nil }

func (f *fakeContainers) set(name, state string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.states[name] = state
}

func (f *fakeContainers) tamper(name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.configs[name] += "lxc.tty.max = 9\n"
}

func (f *fakeContainers) ct1Starts() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.starts[ct1Name]
}

// reboot 模拟宿主机重启：所有容器停止，配置文件还在。
func (f *fakeContainers) reboot() {
	f.mu.Lock()
	defer f.mu.Unlock()
	for name := range maps.Clone(f.states) {
		f.states[name] = stateStopped
	}
}

func (s *suite) waitContainer(t *testing.T, c *computev1alpha1.LXCContainer, ok func(computev1alpha1.LXCContainerStatus) bool) {
	t.Helper()
	eventually(t, 30*time.Second, func() error {
		var got computev1alpha1.LXCContainer
		if err := s.k.Get(t.Context(), client.ObjectKeyFromObject(c), &got); err != nil {
			return err
		}
		if !ok(got.Status) {
			return fmt.Errorf("container %s status: %+v", c.Name, got.Status)
		}
		return nil
	})
}

func (s *suite) rootfsVolume(t *testing.T, name string, reclaim computev1alpha1.ReclaimPolicy) {
	t.Helper()
	create(t, s.k, &computev1alpha1.LibvirtStorageVolume{Name: name, Spec: computev1alpha1.LibvirtStorageVolumeSpec{
		NodeName: node, ReclaimPolicy: reclaim, Size: new(resource.MustParse("1Mi")),
		Libvirt: &computev1alpha1.LibvirtVolumeSource{Pool: localPool}}})
	s.waitVolume(t, name, func(st computev1alpha1.LibvirtStorageVolumeStatus) bool { return st.Path != "" })
}

func newContainer(name, nodeName, rootfs, config string) *computev1alpha1.LXCContainer {
	return &computev1alpha1.LXCContainer{Namespace: corev1.NamespaceDefault, Name: name, Spec: computev1alpha1.LXCContainerSpec{
		Config: config, Rootfs: rootfs, NodeName: nodeName, Networks: []computev1alpha1.ContainerNetwork{{Network: lan}}}}
}

func (s *suite) volumeExists(t *testing.T, name string) bool {
	t.Helper()
	p, err := s.l.LookupStoragePoolByName(localPool)
	if err != nil {
		t.Fatal(err)
	}
	defer free(p.Free)
	v, err := p.LookupStorageVolByName(name)
	if libvirt.IsNotFound(err) {
		return false
	} else if err != nil {
		t.Fatal(err)
	}
	free(v.Free)
	return true
}

func (s *suite) managesContainers(t *testing.T) {
	const rootfs, ct1, libvirtName, base = "ct1-root", "ct1", ct1Name, "lxc.arch = linux64\n"
	s.rootfsVolume(t, rootfs, computev1alpha1.ReclaimDelete)
	c := newContainer(ct1, node, rootfs, base)
	create(t, s.k, c)
	isRunning := func(st computev1alpha1.LXCContainerStatus) bool { return st.State == stateRunning && st.Message == "" }
	s.waitContainer(t, c, isRunning)
	config, _ := containers.Config(libvirtName)
	for _, want := range []string{base + "lxc.uts.name = ct1\n", "lxc.rootfs.path = loop:", "lxc.net.0.link = br-lan\n"} {
		if !strings.Contains(config, want) {
			t.Errorf("config lacks %q:\n%s", want, config)
		}
	}
	s.expectMetric(t, runningMetric, domainLabels(ct1), is(1))
	s.expectMetric(t, shouldRunMetric, domainLabels(ct1), is(1))

	// 带外修改配置文件：改回去。
	containers.tamper(libvirtName)
	eventually(t, 30*time.Second, func() error {
		if got, _ := containers.Config(libvirtName); got != config {
			return errors.New("config drift not restored")
		}
		return nil
	})

	// 容器停止（崩溃或容器内 poweroff，liblxc 区分不了）：拉起。
	before := containers.ct1Starts()
	containers.set(libvirtName, stateStopped)
	eventually(t, 30*time.Second, func() error {
		if containers.ct1Starts() <= before {
			return errors.New("not restarted")
		}
		return nil
	})

	// runState 为 Stopped 时停止后不拉起，也不算应运行。
	patch := client.MergeFrom(c.DeepCopy())
	c.Spec.RunState = computev1alpha1.RunStateStopped
	if err := s.k.Patch(t.Context(), c, patch); err != nil {
		t.Fatal(err)
	}
	s.waitContainer(t, c, func(st computev1alpha1.LXCContainerStatus) bool { return st.ObservedGeneration == c.Generation })
	s.expectMetric(t, shouldRunMetric, domainLabels(ct1), is(0))
	containers.set(libvirtName, stateStopped)
	before = containers.ct1Starts()
	time.Sleep(3 * time.Second)
	if containers.ct1Starts() != before {
		t.Error("a Stopped container was started")
	}

	s.rejectsInvalidContainers(t)

	// 容器还在时，Delete 的卷延后删除；卷的文件不随容器删除，由 reclaimPolicy 决定。
	if err := s.k.Delete(t.Context(), &computev1alpha1.LibvirtStorageVolume{Name: rootfs}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(3 * time.Second)
	if !s.volumeExists(t, rootfs) {
		t.Fatal("rootfs deleted while the container still references it")
	}
	if err := s.k.Delete(t.Context(), c); err != nil {
		t.Fatal(err)
	}
	eventually(t, 30*time.Second, func() error {
		if found, _ := containers.Container(libvirtName); found.Found || s.volumeExists(t, rootfs) {
			return fmt.Errorf("container %+v or its volume still exists", found)
		}
		return nil
	})
	s.expectMetric(t, shouldRunMetric, domainLabels(ct1), absent)

	// 留一个容器给宿主机重启的场景（convergesOffline）。
	s.rootfsVolume(t, ctKeepRoot, computev1alpha1.ReclaimRetain)
	create(t, s.k, newContainer(ctKeep, node, ctKeepRoot, base))
	s.waitContainer(t, newContainer(ctKeep, node, "", ""), isRunning)
}

const ctKeep, ctKeepRoot = "ct-keep", "ct-keep-root"

// ct1Name 是 managesContainers 中容器在节点上的名字。
const ct1Name = "default_ct1"

func (s *suite) rejectsInvalidContainers(t *testing.T) {
	const missing = "no-such-volume"
	// 其他节点的容器不归本节点。
	other := newContainer("elsewhere", "node-b", missing, "lxc.arch = linux64\n")
	create(t, s.k, other)
	// Ayatori 拥有的键写在 config 中：拒绝，不下发。
	s.rootfsVolume(t, "bad-root", computev1alpha1.ReclaimRetain)
	bad := newContainer("bad", node, "bad-root", "lxc.net.0.type = veth\n")
	create(t, s.k, bad)
	s.waitContainer(t, bad, func(st computev1alpha1.LXCContainerStatus) bool {
		return strings.Contains(st.Message, "owned by Ayatori")
	})
	// 依赖稍后出现：rootfs 卷还不存在时报告原因，出现后继续。
	late := newContainer("late", node, "late-root", "lxc.arch = linux64\n")
	create(t, s.k, late)
	s.waitContainer(t, late, func(st computev1alpha1.LXCContainerStatus) bool {
		return strings.Contains(st.Message, "rootfs late-root")
	})
	s.rootfsVolume(t, "late-root", computev1alpha1.ReclaimRetain)
	s.waitContainer(t, late, func(st computev1alpha1.LXCContainerStatus) bool { return st.State == stateRunning })
	if found, _ := containers.Container("default_elsewhere"); found.Found {
		t.Error("defined a container of another node")
	}
	if found, _ := containers.Container("default_bad"); found.Found {
		t.Error("defined a container with an owned key")
	}
	long := newContainer(strings.Repeat("a", 64), node, "x", "x = 1")
	if err := s.k.Create(t.Context(), long); err == nil || !strings.Contains(err.Error(), "at most 63") {
		t.Errorf("name longer than a hostname: %v", err)
	}
	for _, c := range []*computev1alpha1.LXCContainer{other, bad, late} {
		if err := s.k.Delete(t.Context(), c); err != nil {
			t.Fatal(err)
		}
	}
}
