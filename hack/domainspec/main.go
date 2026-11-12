// Command domainspec 处理 LibvirtDomain 的 spec.domain（ADR-0026）：
//
//	domainspec crd <CRD 文件>   把生成的 schema 与默认值写入 controller-gen 生成的 CRD（make manifests 调用）
//	domainspec yaml < dom.xml   把 domain XML（例如 virsh dumpxml 的输出）转换为 spec.domain 的 YAML
//	domainspec xml < dom.yaml   反向转换
package main

import (
	"errors"
	"fmt"
	"io"
	"os"

	"libvirt.org/go/libvirtxml"
	"sigs.k8s.io/yaml"

	"go.ddupan.top/ayatori/internal/compute/domainspec"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "domainspec:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: domainspec crd <file> | yaml | xml")
	}
	switch args[0] {
	case "crd":
		if len(args) != 2 {
			return errors.New("usage: domainspec crd <file>")
		}
		return injectCRD(args[1])
	case "yaml":
		in, err := io.ReadAll(os.Stdin)
		if err != nil {
			return err
		}
		var d libvirtxml.Domain
		if err := d.Unmarshal(string(in)); err != nil {
			return err
		}
		js, err := domainspec.Encode(&d)
		if err != nil {
			return err
		}
		out, err := yaml.JSONToYAML(js)
		if err != nil {
			return err
		}
		_, err = os.Stdout.Write(out)
		return err
	case "xml":
		in, err := io.ReadAll(os.Stdin)
		if err != nil {
			return err
		}
		js, err := yaml.YAMLToJSON(in)
		if err != nil {
			return err
		}
		d, err := domainspec.Decode(js)
		if err != nil {
			return err
		}
		out, err := d.Marshal()
		if err != nil {
			return err
		}
		_, err = fmt.Println(out)
		return err
	}
	return fmt.Errorf("unknown command %q", args[0])
}

// console 是默认的控制台串口：pty，由 Agent 按 alias 经 libvirt 的 console stream 中继（ADR-0016、ADR-0026）。
var console = map[string]any{
	"alias":  map[string]any{"name": "ua-ayatori-console"},
	"source": map[string]any{"pty": map[string]any{}},
	"target": map[string]any{"port": 0},
}

// injectCRD 把 spec.domain 的 schema 写入 CRD，并加上默认值（ADR-0026）：UEFI、ACPI、控制台串口。CRD 的默认值
// 只在字段缺失时生效，因此父对象缺失时的默认值写成完整的对象，不依赖嵌套默认值。
func injectCRD(path string) error {
	raw, err := os.ReadFile(path) //nolint:gosec // 生成工具，路径来自 Makefile
	if err != nil {
		return err
	}
	var crd map[string]any
	if err := yaml.Unmarshal(raw, &crd); err != nil {
		return err
	}
	s := domainspec.Schema()
	props := s["properties"].(map[string]any)
	osSchema := props["os"].(map[string]any)
	osSchema["default"] = map[string]any{"firmware": "efi"}
	osSchema["properties"].(map[string]any)["firmware"].(map[string]any)["default"] = "efi"
	props["features"].(map[string]any)["default"] = map[string]any{"acpi": map[string]any{}}
	devices := props["devices"].(map[string]any)
	devices["default"] = map[string]any{"serials": []any{console}}
	devices["properties"].(map[string]any)["serials"].(map[string]any)["default"] = []any{console}
	s["description"] = "domain 是 libvirtxml.Domain 按 ADR-0026 的规则映射成的对象，交给 libvirt 的就是它；" +
		"Ayatori 只填写自己拥有的字段。用 `go run ./hack/domainspec yaml < dom.xml` 从 XML 转换。"

	versions, _ := dig(crd, "spec", "versions").([]any)
	if len(versions) == 0 {
		return errors.New("CRD has no versions")
	}
	for _, v := range versions {
		spec, ok := dig(v, "schema", "openAPIV3Schema", "properties", "spec", "properties").(map[string]any)
		if !ok {
			return errors.New("CRD has no spec schema")
		}
		if _, ok := spec["domain"]; !ok {
			return errors.New("CRD spec has no domain field")
		}
		spec["domain"] = s
	}
	out, err := yaml.Marshal(crd)
	if err != nil {
		return err
	}
	return writeFile(path, append([]byte("---\n"), out...))
}

func writeFile(path string, data []byte) error {
	return os.WriteFile(path, data, 0o644) //nolint:gosec // 提交进仓库的清单
}

func dig(x any, path ...string) any {
	for _, p := range path {
		m, ok := x.(map[string]any)
		if !ok {
			return nil
		}
		x = m[p]
	}
	return x
}
