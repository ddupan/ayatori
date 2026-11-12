package domainspec

import (
	"cmp"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"libvirt.org/go/libvirtxml"
)

func TestKey(t *testing.T) {
	for in, want := range map[string]string{
		"Name": "name", "UUID": "uuid", "VCPU": "vcpu", "D": "d", "IOThreads": "ioThreads", "IOThreadIDs": "ioThreadIDs",
		"TPMs": "tpms", "CPUs": "cpus", "IOMMUs": "iommus", "SHA256": "sha256", "PS2": "ps2", "VEPA8021QBG": "vepa8021qbg",
		"NVRam": "nvRam", "MCast": "mCast", "VNTag8011QBH": "vnTag8011QBH", "MemoryBacking": "memoryBacking",
	} {
		if got := Key(in); got != want {
			t.Errorf("Key(%s) = %s, want %s", in, got, want)
		}
	}
}

// TestBijection 检查映射在整个 libvirtxml domain 模型上是一一对应的：每个结构体（包括展平提升的字段）内 key
// 唯一，且没有映射不了的字段类型。升级 libvirtxml 后它保证新增或改名的字段不会撞名。
func TestBijection(t *testing.T) {
	seen := map[reflect.Type]bool{}
	var walk func(reflect.Type)
	walk = func(tp reflect.Type) {
		for tp.Kind() == reflect.Pointer || tp.Kind() == reflect.Slice {
			tp = tp.Elem()
		}
		if tp.Kind() != reflect.Struct || seen[tp] {
			return
		}
		seen[tp] = true
		keys := map[string]string{}
		for _, f := range fields(tp) {
			k := Key(f.Name)
			if other, ok := keys[k]; ok {
				t.Errorf("%s: %s and %s both map to %q", tp.Name(), other, f.Name, k)
			}
			keys[k] = f.Name
			walk(f.Type)
		}
	}
	walk(reflect.TypeFor[libvirtxml.Domain]())
	Schema() // 有不支持的字段类型时 panic
	if len(seen) < 500 {
		t.Fatalf("walked only %d struct types", len(seen))
	}
}

// TestRoundTrip：XML → libvirtxml → spec.domain → libvirtxml → XML 与直接经 libvirtxml 的结果逐字节相同。
// 升级 libvirtxml 时另用 libvirt 源码的 tests/qemuxmlconfdata 全量跑一次（ADR-0026）：
// SAMPLES=<dir> go test -run TestRoundTrip ./internal/compute/domainspec
func TestRoundTrip(t *testing.T) {
	dir := cmp.Or(os.Getenv("SAMPLES"), "testdata")
	files, _ := filepath.Glob(filepath.Join(dir, "*.xml"))
	if len(files) == 0 {
		t.Fatal("no samples")
	}
	parsed := 0
	for _, f := range files {
		raw, err := os.ReadFile(f) //nolint:gosec // 测试样本
		if err != nil {
			t.Fatal(err)
		}
		var d libvirtxml.Domain
		if d.Unmarshal(string(raw)) != nil {
			continue // libvirt 的语料中有少量故意写错的输入
		}
		parsed++
		// libvirtxml 的 DomainNVRam.UnmarshalXML 本意是有 <source> 时清空文本内容，但清空发生在把解码结果赋回之前，
		// 没有生效，<nvram> 内的空白因此留在 NVRam 中，序列化时又不会写出。这里补上它本来要做的事。
		if d.OS != nil && d.OS.NVRam != nil && d.OS.NVRam.Source != nil {
			d.OS.NVRam.NVRam = ""
		}
		want, err := d.Marshal()
		if err != nil {
			continue
		}
		js, err := Encode(&d)
		if err != nil {
			t.Fatalf("%s: encode: %v", f, err)
		}
		back, err := Decode(js)
		if err != nil {
			t.Errorf("%s: decode: %v", f, err)
			continue
		}
		got, err := Marshal(back)
		if err != nil {
			t.Errorf("%s: marshal: %v", f, err)
			continue
		}
		if got != want {
			t.Errorf("%s: round trip differs:\n%s\n---\n%s", f, want, got)
		}
	}
	t.Logf("%d of %d samples parsed and round-tripped", parsed, len(files))
}

func TestRejects(t *testing.T) {
	for name, js := range map[string]string{
		"union with two branches": `{"type":"kvm","devices":{"disks":[{"source":{"file":{"file":"/a"},"block":{"dev":"/b"}}}]}}`,
		"unknown field":           `{"devices":{"disks":[{"sauce":{}}]}}`,
		"wrong type":              `{"vcpu":{"value":"two"}}`,
		"negative unsigned":       `{"vcpu":{"value":-1}}`,
	} {
		d, err := Decode([]byte(js))
		if err == nil {
			_, err = Marshal(d)
		}
		if err == nil {
			t.Errorf("%s: accepted", name)
		} else {
			t.Logf("%s: %v", name, err)
		}
	}
	// 没写 source 的 disk 读回时 libvirtxml 补上 source.file: {}，这不是丢失。
	d, err := Decode([]byte(`{"type":"kvm","devices":{"disks":[{"device":"cdrom","target":{"dev":"sda","bus":"sata"}}]}}`))
	if err == nil {
		_, err = Marshal(d)
	}
	if err != nil {
		t.Errorf("disk without source: %v", err)
	}
}

func TestSchemaUnions(t *testing.T) {
	s := Schema()
	source := dig(t, s, "devices", "disks", "source")
	if one := fmt.Sprint(source["oneOf"]); !strings.Contains(one, "required:[file]") || !strings.Contains(one, "required:[block]") {
		t.Errorf("disk source lacks the union constraint: %v", one)
	}
	if _, ok := dig(t, s, "devices", "disks")["oneOf"]; ok {
		t.Error("disk itself is not a union")
	}
}

func dig(t *testing.T, s map[string]any, path ...string) map[string]any {
	t.Helper()
	for _, p := range path {
		if items, ok := s["items"].(map[string]any); ok {
			s = items
		}
		next, ok := s["properties"].(map[string]any)[p].(map[string]any)
		if !ok {
			t.Fatalf("no %s in schema", p)
		}
		s = next
	}
	if items, ok := s["items"].(map[string]any); ok {
		s = items
	}
	return s
}
