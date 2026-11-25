package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"strings"
	"text/tabwriter"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/duration"
	"k8s.io/cli-runtime/pkg/printers"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/yaml"
)

// Resource 是 CLI 的名词与背后 API 资源的对应。名词是 Ayatori 的概念，不是 CRD 的名字（ADR-0027）：
// 后端换了实现，命令不变。
type Resource struct {
	Noun       string
	Short      string
	GVR        schema.GroupVersionResource
	Namespaced bool
}

// Resources 是面向用户的资源。db 是用户的申请（PostgreSQLTenant，ADR-0009）；Database、Instance 等
// 管理员资源以后放进 adm。
var Resources = []Resource{
	{Noun: "db", Short: "PostgreSQL databases", Namespaced: true,
		GVR: schema.GroupVersionResource{Group: "database.ayatori.ddupan.top", Version: "v1alpha1", Resource: "postgresqltenants"}},
	{Noun: "lb", Short: "L4 load balancers", Namespaced: true,
		GVR: schema.GroupVersionResource{Group: "network.ayatori.ddupan.top", Version: "v1alpha1", Resource: "loadbalancers"}},
}

// ListOptions：Namespace 为空表示所有 namespace；Name 非空时只列这一个（不给 namespace 时见 Locate）；
// Output 为空或 wide 时打印表格，否则见 objectPrinter。
type ListOptions struct {
	Namespace string
	Name      string
	Output    string
}

// List 打印资源列表。表格由 apiserver 按 CRD 的 printer columns 生成（与 kubectl get 相同），
// CLI 不自己定义列。
func List(ctx context.Context, config *rest.Config, r Resource, opts ListOptions, out io.Writer) error {
	if opts.Name != "" {
		namespace, err := Locate(ctx, config, r, opts.Namespace, opts.Name)
		if err != nil {
			return err
		}
		opts.Namespace = namespace
	}
	if opts.Output != "" && opts.Output != "wide" {
		printer, err := objectPrinter(opts.Output)
		if err != nil {
			return err
		}
		client, err := dynamic.NewForConfig(config)
		if err != nil {
			return err
		}
		resource := client.Resource(r.GVR).Namespace(opts.Namespace)
		if opts.Name != "" {
			object, err := resource.Get(ctx, opts.Name, metav1.GetOptions{})
			if err != nil {
				return err
			}
			return printer.PrintObj(object, out)
		}
		list, err := resource.List(ctx, metav1.ListOptions{})
		if err != nil {
			return allNamespacesHint(err, opts.Namespace)
		}
		return printer.PrintObj(list, out)
	}
	table, err := fetchTable(ctx, config, r, opts.Namespace, opts.Name)
	if err != nil {
		return allNamespacesHint(err, opts.Namespace)
	}
	if len(table.Rows) == 0 {
		_, err := fmt.Fprintf(out, "No %s found.\n", r.Short)
		return err
	}
	return printers.NewTablePrinter(printers.PrintOptions{
		Wide: opts.Output == "wide", WithNamespace: r.Namespaced && opts.Namespace == "",
	}).PrintObj(table, out)
}

// Locate 返回名为 name 的对象所在的 namespace。namespace 已给出时原样返回；否则跨所有 namespace 按名字
// 查找，唯一时用它，多个时列出候选要求 -n。namespace 是应用隔离而不是用户的工作上下文，所以 CLI 不保存
// "当前 namespace"（ADR-0027）。
func Locate(ctx context.Context, config *rest.Config, r Resource, namespace, name string) (string, error) {
	if namespace != "" || !r.Namespaced {
		return namespace, nil
	}
	client, err := dynamic.NewForConfig(config)
	if err != nil {
		return "", err
	}
	list, err := client.Resource(r.GVR).List(ctx, metav1.ListOptions{FieldSelector: "metadata.name=" + name})
	if err != nil {
		return "", allNamespacesHint(err, "")
	}
	switch len(list.Items) {
	case 0:
		return "", apierrors.NewNotFound(r.GVR.GroupResource(), name)
	case 1:
		return list.Items[0].GetNamespace(), nil
	}
	candidates := make([]string, 0, len(list.Items))
	for _, item := range list.Items {
		candidates = append(candidates, item.GetNamespace()+"/"+name)
	}
	return "", fmt.Errorf("%s %q exists in several namespaces (%s): choose one with -n",
		r.Noun, name, strings.Join(candidates, ", "))
}

// allNamespacesHint：跨 namespace 列出需要集群范围的 list 权限，只能看部分 namespace 的用户得用 -n。
func allNamespacesHint(err error, namespace string) error {
	if namespace == "" && apierrors.IsForbidden(err) {
		return fmt.Errorf("%w\nyou cannot list across all namespaces: name one with -n", err)
	}
	return err
}

// objectPrinter：json、yaml 与 jsonpath=<模板>，都去掉 managedFields。
func objectPrinter(output string) (printers.ResourcePrinter, error) {
	var printer printers.ResourcePrinter
	switch {
	case output == "json":
		printer = &printers.JSONPrinter{}
	case output == "yaml":
		printer = &printers.YAMLPrinter{}
	case strings.HasPrefix(output, "jsonpath="):
		p, err := printers.NewJSONPathPrinter(strings.TrimPrefix(output, "jsonpath="))
		if err != nil {
			return nil, err
		}
		p.AllowMissingKeys(true)
		printer = p
	default:
		return nil, fmt.Errorf("unknown output %q: want wide, json, yaml or jsonpath=<template>", output)
	}
	return &printers.OmitManagedFieldsPrinter{Delegate: printer}, nil
}

// fetchTable 以 kubectl 的方式请求 Table；行里的对象是 PartialObjectMetadata，表格打印器从中取 namespace。
func fetchTable(ctx context.Context, config *rest.Config, r Resource, namespace, name string) (*metav1.Table, error) {
	c := rest.CopyConfig(config)
	c.GroupVersion = &schema.GroupVersion{Group: r.GVR.Group, Version: r.GVR.Version}
	c.APIPath = "/apis"
	c.NegotiatedSerializer = clientgoscheme.Codecs.WithoutConversion()
	client, err := rest.RESTClientFor(c)
	if err != nil {
		return nil, err
	}
	req := client.Get().NamespaceIfScoped(namespace, r.Namespaced && namespace != "").Resource(r.GVR.Resource).
		SetHeader("Accept", "application/json;as=Table;v=v1;g=meta.k8s.io")
	if name != "" {
		req = req.Name(name)
	}
	raw, err := req.Do(ctx).Raw()
	if err != nil {
		return nil, err
	}
	var table metav1.Table
	if err := json.Unmarshal(raw, &table); err != nil {
		return nil, err
	}
	for i := range table.Rows {
		object := &unstructured.Unstructured{}
		if err := json.Unmarshal(table.Rows[i].Object.Raw, &object.Object); err != nil {
			return nil, err
		}
		table.Rows[i].Object.Object = object
	}
	return &table, nil
}

// Describe 打印一个对象：名字、spec、status、conditions 与相关 Event。不打印 UID、resourceVersion、
// managedFields、owner reference 这类 Kubernetes 细节（ADR-0027 原则 2），需要时用 `list NAME -o yaml`。
func Describe(ctx context.Context, config *rest.Config, r Resource, namespace, name string, out io.Writer) error {
	namespace, err := Locate(ctx, config, r, namespace, name)
	if err != nil {
		return err
	}
	client, err := dynamic.NewForConfig(config)
	if err != nil {
		return err
	}
	object, err := client.Resource(r.GVR).Namespace(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return err
	}
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return err
	}
	events, err := clientset.CoreV1().Events(namespace).List(ctx, metav1.ListOptions{
		FieldSelector: "involvedObject.uid=" + string(object.GetUID())})
	if err != nil {
		return err
	}

	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	line := func(format string, args ...any) { _, _ = fmt.Fprintf(w, format+"\n", args...) }
	line("Name:\t%s", object.GetName())
	if r.Namespaced {
		line("Namespace:\t%s", object.GetNamespace())
	}
	line("Created:\t%s ago", age(object.GetCreationTimestamp().Time))
	if labels := object.GetLabels(); len(labels) > 0 {
		keys := make([]string, 0, len(labels))
		for k := range labels {
			keys = append(keys, k+"="+labels[k])
		}
		slices.Sort(keys)
		line("Labels:\t%s", strings.Join(keys, ", "))
	}
	status, _ := object.Object["status"].(map[string]any)
	conditions, _ := status["conditions"].([]any)
	others := map[string]any{}
	for k, v := range status {
		if k != "conditions" {
			others[k] = v
		}
	}
	for _, section := range []struct {
		title string
		value any
	}{{"Spec", object.Object["spec"]}, {"Status", others}} {
		if m, ok := section.value.(map[string]any); !ok || len(m) == 0 {
			continue
		}
		data, err := yaml.Marshal(section.value)
		if err != nil {
			return err
		}
		line("%s:", section.title)
		for l := range strings.SplitSeq(strings.TrimRight(string(data), "\n"), "\n") {
			line("  %s", l)
		}
	}
	if len(conditions) > 0 {
		line("Conditions:")
		line("  TYPE\tSTATUS\tREASON\tAGE\tMESSAGE")
		for _, c := range conditions {
			m, _ := c.(map[string]any)
			changed, _ := time.Parse(time.RFC3339, fmt.Sprint(m["lastTransitionTime"]))
			line("  %v\t%v\t%v\t%s\t%v", m["type"], m["status"], m["reason"], age(changed), m["message"])
		}
	}
	if len(events.Items) > 0 {
		slices.SortFunc(events.Items, func(a, b corev1.Event) int { return eventTime(a).Compare(eventTime(b)) })
		line("Events:")
		line("  AGE\tTYPE\tREASON\tFROM\tMESSAGE")
		for _, e := range events.Items {
			from := e.ReportingController
			if from == "" {
				from = e.Source.Component
			}
			line("  %s\t%s\t%s\t%s\t%s", age(eventTime(e)), e.Type, e.Reason, from, strings.TrimSpace(e.Message))
		}
	}
	return w.Flush()
}

func eventTime(e corev1.Event) time.Time {
	for _, t := range []time.Time{e.LastTimestamp.Time, e.EventTime.Time, e.FirstTimestamp.Time} {
		if !t.IsZero() {
			return t
		}
	}
	return e.CreationTimestamp.Time
}

func age(t time.Time) string {
	if t.IsZero() {
		return "<unknown>"
	}
	return duration.HumanDuration(time.Since(t))
}
