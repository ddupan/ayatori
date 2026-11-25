// Package domainspec 在 libvirtxml.Domain 与 LibvirtDomain 的 spec.domain（JSON）之间做机械的一一映射（ADR-0026）。
//
// 规则：
//   - key 是 Go 字段名的 lowerCamel，见 Key；
//   - 值嵌入的结构体展平到外层；
//   - nil 指针省略，非 nil 指针一律保留：空结构体写成 {}，零值标量照写；值类型的零值与"没写"等价；
//   - 切片映射为列表，保持顺序。
//
// 映射的对象是 libvirtxml 的模型：libvirtxml 序列化时会丢掉的内容（例如联合体的第二个分支），Marshal 拒绝。
package domainspec

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"strconv"
	"unicode"

	"libvirt.org/go/libvirtxml"
)

// Key 把 Go 字段名映射为 lowerCamel。开头由大写字母和数字组成的缩写整体小写；缩写后紧跟小写字母时，
// 最后一个大写字母属于下一个词；紧跟复数 s 时缩写整体小写：UUID→uuid、IOThreads→ioThreads、TPMs→tpms、
// SHA256→sha256。
func Key(name string) string {
	r := []rune(name)
	i := 0
	for i < len(r) && (unicode.IsUpper(r[i]) || unicode.IsDigit(r[i])) {
		i++
	}
	plural := i < len(r) && r[i] == 's' && (i+1 == len(r) || unicode.IsUpper(r[i+1]))
	if i > 1 && i < len(r) && !plural {
		i--
	}
	for j := range i {
		r[j] = unicode.ToLower(r[j])
	}
	return string(r)
}

// fields 列出结构体映射的字段；值嵌入的结构体展平到外层（Index 是完整路径）。
func fields(t reflect.Type) []reflect.StructField {
	var out []reflect.StructField
	for i := range t.NumField() {
		f := t.Field(i)
		if !f.IsExported() || f.Name == "XMLName" {
			continue
		}
		if f.Anonymous && f.Type.Kind() == reflect.Struct {
			for _, inner := range fields(f.Type) {
				inner.Index = append([]int{i}, inner.Index...)
				out = append(out, inner)
			}
			continue
		}
		out = append(out, f)
	}
	return out
}

// Encode 把 domain 映射为 spec.domain 的 JSON。
func Encode(d *libvirtxml.Domain) ([]byte, error) {
	tree, _ := toTree(reflect.ValueOf(d))
	return json.Marshal(tree)
}

// Decode 把 spec.domain 的 JSON 映射为 domain。
func Decode(raw []byte) (*libvirtxml.Domain, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var tree any
	if err := dec.Decode(&tree); err != nil {
		return nil, err
	}
	var d libvirtxml.Domain
	if err := fromTree(tree, reflect.ValueOf(&d).Elem(), "domain"); err != nil {
		return nil, err
	}
	return &d, nil
}

// Marshal 把 domain 序列化为 XML，并检查 libvirtxml 没有丢掉任何内容（例如联合体的第二个分支）：
// 交给 libvirt 的必须就是写下的内容。
func Marshal(d *libvirtxml.Domain) (string, error) {
	xml, err := d.Marshal()
	if err != nil {
		return "", err
	}
	var back libvirtxml.Domain
	if err := back.Unmarshal(xml); err != nil {
		return "", err
	}
	want, _ := toTree(reflect.ValueOf(d))
	got, _ := toTree(reflect.ValueOf(&back))
	if path := diff(want, got, "domain"); path != "" {
		return "", fmt.Errorf("%s cannot be expressed in libvirt XML (for a union, set only one branch)", path)
	}
	return xml, nil
}

// diff 返回 a 中在 b 里丢失或不同的第一处路径；没有时返回空。b 多出的内容不算：libvirtxml 解析时会补上
// 判别值对应的空分支（没写 source 的 disk 读回为 source.file: {}），那不是丢失。
func diff(a, b any, path string) string {
	switch x := a.(type) {
	case map[string]any:
		y, ok := b.(map[string]any)
		if !ok {
			return path
		}
		for k, v := range x {
			if p := diff(v, y[k], path+"."+k); p != "" {
				return p
			}
		}
		return ""
	case []any:
		y, ok := b.([]any)
		if !ok || len(x) != len(y) {
			return path
		}
		for i := range x {
			if p := diff(x[i], y[i], fmt.Sprintf("%s[%d]", path, i)); p != "" {
				return p
			}
		}
		return ""
	default:
		if !reflect.DeepEqual(a, b) {
			return path
		}
		return ""
	}
}

// toTree 返回 v 的树形表示；第二个返回值表示值不为空。
func toTree(v reflect.Value) (any, bool) {
	switch v.Kind() {
	case reflect.Pointer:
		if v.IsNil() {
			return nil, false
		}
		out, ok := toTree(v.Elem())
		switch {
		case ok:
			return out, true
		case v.Elem().Kind() == reflect.Struct:
			return map[string]any{}, true
		default:
			return v.Elem().Interface(), true
		}
	case reflect.Struct:
		m := map[string]any{}
		for _, f := range fields(v.Type()) {
			if x, ok := toTree(v.FieldByIndex(f.Index)); ok {
				m[Key(f.Name)] = x
			}
		}
		return m, len(m) > 0
	case reflect.Slice:
		if v.Len() == 0 {
			return nil, false
		}
		out := make([]any, v.Len())
		for i := range v.Len() {
			x, _ := toTree(v.Index(i))
			if x == nil {
				x = map[string]any{}
			}
			out[i] = x
		}
		return out, true
	default:
		if v.IsZero() {
			return nil, false
		}
		return v.Interface(), true
	}
}

func fromTree(tree any, v reflect.Value, path string) error {
	if tree == nil {
		return nil
	}
	switch v.Kind() {
	case reflect.Pointer:
		v.Set(reflect.New(v.Type().Elem()))
		return fromTree(tree, v.Elem(), path)
	case reflect.Struct:
		m, ok := tree.(map[string]any)
		if !ok {
			return fmt.Errorf("%s: want an object", path)
		}
		byKey := map[string][]int{}
		for _, f := range fields(v.Type()) {
			byKey[Key(f.Name)] = f.Index
		}
		for k, x := range m {
			index, ok := byKey[k]
			if !ok {
				return fmt.Errorf("%s.%s: unknown field", path, k)
			}
			if err := fromTree(x, v.FieldByIndex(index), path+"."+k); err != nil {
				return err
			}
		}
		return nil
	case reflect.Slice:
		l, ok := tree.([]any)
		if !ok {
			return fmt.Errorf("%s: want an array", path)
		}
		s := reflect.MakeSlice(v.Type(), len(l), len(l))
		for i, x := range l {
			if err := fromTree(x, s.Index(i), fmt.Sprintf("%s[%d]", path, i)); err != nil {
				return err
			}
		}
		v.Set(s)
		return nil
	case reflect.String:
		s, ok := tree.(string)
		if !ok {
			return fmt.Errorf("%s: want a string", path)
		}
		v.SetString(s)
		return nil
	case reflect.Bool:
		b, ok := tree.(bool)
		if !ok {
			return fmt.Errorf("%s: want a boolean", path)
		}
		v.SetBool(b)
		return nil
	}
	n, ok := tree.(json.Number)
	if !ok {
		return fmt.Errorf("%s: want a number", path)
	}
	var err error
	switch v.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		var i int64
		if i, err = strconv.ParseInt(n.String(), 10, v.Type().Bits()); err == nil {
			v.SetInt(i)
		}
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		var u uint64
		if u, err = strconv.ParseUint(n.String(), 10, v.Type().Bits()); err == nil {
			v.SetUint(u)
		}
	case reflect.Float32, reflect.Float64:
		var f float64
		if f, err = strconv.ParseFloat(n.String(), v.Type().Bits()); err == nil {
			v.SetFloat(f)
		}
	default:
		return fmt.Errorf("%s: unsupported %s", path, v.Kind())
	}
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	return nil
}
