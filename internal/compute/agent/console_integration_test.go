//go:build integration

package agent_test

import (
	"errors"
	"fmt"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"

	computev1alpha1 "go.ddupan.top/ayatori/api/compute/v1alpha1"
	"go.ddupan.top/ayatori/internal/compute/console"
)

// test 驱动没有真正的 QEMU：没有字符设备的流，也没有 VNC/SPICE socket。这里验证中继选择实例的规则；串口与
// VNC/SPICE 的实际转发在 Dev 节点上验证（ADR-0016）。
func (s *suite) relaysConsoles(t *testing.T) {
	ctx := t.Context()
	open := func(name, kind string) error {
		c, err := s.agent.OpenConsole(ctx, corev1.NamespaceDefault, name, kind)
		if err == nil {
			_ = c.Close()
		}
		return err
	}
	if err := open("nobody", console.Serial); !errors.Is(err, console.ErrNotFound) {
		t.Errorf("unknown instance: %v", err)
	}
	// libvirt 中有同名 domain，但它不归 Ayatori。
	ghost, err := s.l.DomainCreateXML(`<domain type='test'><name>default_ghost</name><memory unit='KiB'>65536</memory><os><type>hvm</type></os></domain>`, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ghost.Destroy(); free(ghost.Free) }()
	if err := open("ghost", console.Serial); !errors.Is(err, console.ErrNotFound) {
		t.Errorf("domain not managed by Ayatori: %v", err)
	}
	// vm2 运行中，带 CRD 默认的控制台串口，没有图形控制台。
	if err := open(s.vm2.Name, console.VNC); !errors.Is(err, console.ErrNoDevice) {
		t.Errorf("vnc without graphics: %v", err)
	}
	if err := open(s.vm2.Name, console.Serial); err == nil || errors.Is(err, console.ErrNoDevice) || errors.Is(err, console.ErrNotFound) {
		t.Errorf("serial should reach libvirt's console stream (unsupported by the test driver): %v", err)
	} else {
		t.Logf("serial on the test driver: %v", err)
	}

	off := &computev1alpha1.LibvirtDomain{Namespace: corev1.NamespaceDefault, Name: "console-off", Spec: computev1alpha1.LibvirtDomainSpec{
		NodeName: node, RunState: computev1alpha1.RunStateStopped, Domain: domainSpec(t, `<serial type='pty'><target port='0'/><alias name='ua-ayatori-console'/></serial>`)}}
	create(t, s.k, off)
	defer func() { _ = s.k.Delete(ctx, off) }()
	eventually(t, 30*time.Second, func() error {
		if err := open(off.Name, console.Serial); !errors.Is(err, console.ErrNotRunning) {
			return fmt.Errorf("stopped instance: %w", err)
		}
		return nil
	})
}
