package cli

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

const ns, web = "team-a", "web"

// 对装了 Ayatori CRD 的真实 apiserver 验证 list 与 describe：表格列来自 CRD 的 printer columns，
// -A 加 namespace 列，-o 输出不含 managedFields，describe 带 conditions 与 Event。
func TestListAndDescribe(t *testing.T) {
	env := &envtest.Environment{CRDDirectoryPaths: []string{filepath.Join("..", "..", "config", "crd", "bases")},
		ErrorIfCRDPathMissing: true}
	config, err := env.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = env.Stop() })
	ctx := t.Context()
	lb, db := resource(t, "lb"), resource(t, "db")
	seed(t, ctx, config, lb, db)

	run := func(t *testing.T, f func(*bytes.Buffer) error) string {
		t.Helper()
		var out bytes.Buffer
		if err := f(&out); err != nil {
			t.Fatal(err)
		}
		return out.String()
	}
	list := func(t *testing.T, r Resource, opts ListOptions) string {
		return run(t, func(out *bytes.Buffer) error { return List(ctx, config, r, opts, out) })
	}

	t.Run("the table has the CRD's printer columns", func(t *testing.T) {
		out := list(t, lb, ListOptions{Namespace: ns})
		for _, want := range []string{"NAME", "ADDRESS", "PROGRAMMED", web, "10.62.1.10", "True"} {
			if !strings.Contains(out, want) {
				t.Errorf("missing %q in\n%s", want, out)
			}
		}
		if strings.Contains(out, "NAMESPACE") {
			t.Errorf("namespace column in a single-namespace list:\n%s", out)
		}
	})

	t.Run("all namespaces adds the namespace column", func(t *testing.T) {
		out := list(t, lb, ListOptions{})
		if !strings.Contains(out, "NAMESPACE") || !strings.Contains(out, ns) {
			t.Errorf("no namespace column in\n%s", out)
		}
	})

	t.Run("an empty namespace says so", func(t *testing.T) {
		if out := list(t, lb, ListOptions{Namespace: "default"}); out != "No L4 load balancers found.\n" {
			t.Errorf("got %q", out)
		}
	})

	t.Run("yaml and jsonpath leave out managedFields", func(t *testing.T) {
		out := list(t, lb, ListOptions{Namespace: ns, Output: "yaml"})
		if !strings.Contains(out, "kind: LoadBalancer") || strings.Contains(out, "managedFields") {
			t.Errorf("yaml:\n%s", out)
		}
		if out := list(t, db, ListOptions{Namespace: ns, Output: "jsonpath={.items[*].metadata.name}"}); out != "app" {
			t.Errorf("jsonpath = %q", out)
		}
		out = list(t, lb, ListOptions{Namespace: ns, Name: web, Output: "json"})
		if !strings.Contains(out, `"name": "web"`) || strings.Contains(out, `"items"`) {
			t.Errorf("single object json:\n%s", out)
		}
		if _, err := objectPrinter("table"); err == nil {
			t.Error("unknown output accepted")
		}
	})

	t.Run("describe shows spec, status, conditions and events", func(t *testing.T) {
		out := run(t, func(out *bytes.Buffer) error { return Describe(ctx, config, lb, ns, web, out) })
		for _, want := range []string{"Name:", "Namespace:", ns, "Spec:", "backendRef:", "Status:", "address: 10.62.1.10",
			"Conditions:", "Programmed", "Allocated", "Events:", "AddressAllocated", "allocated 10.62.1.10"} {
			if !strings.Contains(out, want) {
				t.Errorf("missing %q in\n%s", want, out)
			}
		}
		for _, noise := range []string{"uid", "resourceVersion", "managedFields", "lastTransitionTime"} {
			if strings.Contains(out, noise) {
				t.Errorf("describe shows %q:\n%s", noise, out)
			}
		}
	})

	t.Run("describe of a missing object is an error", func(t *testing.T) {
		if err := Describe(ctx, config, db, ns, "nope", &bytes.Buffer{}); err == nil {
			t.Error("no error")
		}
	})
}

func resource(t *testing.T, noun string) Resource {
	t.Helper()
	for _, r := range Resources {
		if r.Noun == noun {
			return r
		}
	}
	t.Fatalf("no resource %q", noun)
	return Resource{}
}

func seed(t *testing.T, ctx context.Context, config *rest.Config, lb, db Resource) {
	t.Helper()
	client, err := dynamic.NewForConfig(config)
	if err != nil {
		t.Fatal(err)
	}
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := clientset.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{Name: ns},
		metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	object := func(kind, name string, spec map[string]any) *unstructured.Unstructured {
		u := &unstructured.Unstructured{Object: map[string]any{"spec": spec}}
		u.SetKind(kind)
		u.SetName(name)
		u.SetNamespace(ns)
		return u
	}
	balancer := object("LoadBalancer", web, map[string]any{"ports": []any{map[string]any{"port": int64(443),
		"backendRef": map[string]any{"name": web}}}})
	balancer.SetAPIVersion(lb.GVR.GroupVersion().String())
	created, err := client.Resource(lb.GVR).Namespace(ns).Create(ctx, balancer, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	created.Object["status"] = map[string]any{"address": "10.62.1.10", "conditions": []any{map[string]any{
		"type": "Programmed", "status": "True", "reason": "Allocated", "message": "address allocated",
		"lastTransitionTime": time.Now().UTC().Format(time.RFC3339), "observedGeneration": int64(1)}}}
	if _, err := client.Resource(lb.GVR).Namespace(ns).UpdateStatus(ctx, created, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	app := object("PostgreSQLTenant", "app", map[string]any{"provision": map[string]any{"instanceRef": map[string]any{"name": "pg"}}})
	app.SetAPIVersion(db.GVR.GroupVersion().String())
	if _, err := client.Resource(db.GVR).Namespace(ns).Create(ctx, app, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	now := metav1.Now()
	if _, err := clientset.CoreV1().Events(ns).Create(ctx, &corev1.Event{
		GenerateName: web + "-",
		InvolvedObject: corev1.ObjectReference{Kind: "LoadBalancer", Namespace: ns, Name: web, UID: created.GetUID(),
			APIVersion: lb.GVR.GroupVersion().String()},
		Type: corev1.EventTypeNormal, Reason: "AddressAllocated", Message: "allocated 10.62.1.10",
		Source: corev1.EventSource{Component: "ayatori-loadbalancer"}, FirstTimestamp: now, LastTimestamp: now,
	}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
}
