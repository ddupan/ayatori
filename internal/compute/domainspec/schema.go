package domainspec

import (
	"fmt"
	"reflect"
	"slices"

	"libvirt.org/go/libvirtxml"
)

const (
	typ = "type"
	// 每个字符串与列表都有上限，否则 apiserver 估算列表上 CEL 规则的成本时会超限。
	maxLength = 4096
	maxItems  = 256
)

// Schema 返回 spec.domain 的 OpenAPI v3 schema。递归处（只有磁盘的 backingStore）截断为任意对象。
//
// 联合体：libvirtxml 用 xml:"-" 标记、由自定义 marshaler 按判别值处理的指针字段是同一个联合体的分支，
// 最多设置其中一个，schema 中用 oneOf 表达。其余会被 libvirtxml 丢弃的组合由 Marshal 拒绝。
func Schema() map[string]any {
	return schema(reflect.TypeFor[libvirtxml.Domain](), map[reflect.Type]bool{})
}

func schema(t reflect.Type, stack map[reflect.Type]bool) map[string]any {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	switch t.Kind() {
	case reflect.String:
		return map[string]any{typ: "string", "maxLength": maxLength}
	case reflect.Bool:
		return map[string]any{typ: "boolean"}
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return map[string]any{typ: "integer"}
	case reflect.Float32, reflect.Float64:
		return map[string]any{typ: "number"}
	case reflect.Slice:
		return map[string]any{typ: "array", "maxItems": maxItems, "items": schema(t.Elem(), stack)}
	case reflect.Struct:
		if stack[t] {
			return map[string]any{typ: "object", "x-kubernetes-preserve-unknown-fields": true}
		}
		stack[t] = true
		defer delete(stack, t)
		props := map[string]any{}
		var branches []string
		for _, f := range fields(t) {
			props[Key(f.Name)] = schema(f.Type, stack)
			if f.Tag.Get("xml") == "-" && f.Type.Kind() == reflect.Pointer {
				branches = append(branches, Key(f.Name))
			}
		}
		s := map[string]any{typ: "object", "properties": props}
		if len(branches) > 1 {
			// 至多一个分支：恰好匹配下面的一项。逐对禁止要 n(n-1)/2 项，chardev 的 14 个分支每处就是 91 项。
			var each []any
			for _, b := range branches {
				each = append(each, map[string]any{"required": []any{b}})
			}
			s["oneOf"] = append(slices.Clone(each), map[string]any{"not": map[string]any{"anyOf": each}})
		}
		return s
	}
	panic(fmt.Sprintf("domainspec: unsupported field type %s", t))
}
