package v1alpha1_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	executionv1alpha1 "git.ddupan.top/panxiao81/ayatori/api/execution/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

func TestJobCRDValidation(t *testing.T) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("KUBEBUILDER_ASSETS is unset; run make test to execute API integration tests")
	}

	scheme := runtime.NewScheme()
	if err := executionv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}

	crdPath, err := filepath.Abs("../../../config/crd/bases")
	if err != nil {
		t.Fatal(err)
	}
	environment := &envtest.Environment{CRDDirectoryPaths: []string{crdPath}}
	config, err := environment.Start()
	if err != nil {
		t.Fatalf("start envtest: %v", err)
	}
	t.Cleanup(func() {
		if err := environment.Stop(); err != nil {
			t.Errorf("stop envtest: %v", err)
		}
	})

	client, err := ctrlclient.New(config, ctrlclient.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	//nolint:modernize // ObjectMeta is promoted through embedded TypeMeta; embedlit produces invalid Go here.
	namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "api-validation"}}
	if err := client.Create(ctx, namespace); err != nil {
		t.Fatal(err)
	}

	t.Run("defaults desired state", func(t *testing.T) {
		job := validJob("defaults")
		if err := client.Create(ctx, job); err != nil {
			t.Fatal(err)
		}
		if job.Spec.DesiredState != executionv1alpha1.JobDesiredStateRunning {
			t.Fatalf("desiredState = %q, want Running", job.Spec.DesiredState)
		}
	})

	t.Run("rejects ambiguous environment value", func(t *testing.T) {
		literal := "visible"
		job := validJob("invalid-env")
		job.Spec.Task.Env = []executionv1alpha1.EnvVar{{
			Name:  "TOKEN",
			Value: &literal,
			ValueFrom: &executionv1alpha1.EnvVarSource{
				//nolint:modernize // LocalObjectReference is an embedded Kubernetes API field.
				SecretKeyRef: &corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "token"}, Key: "value"},
			},
		}}
		if err := client.Create(ctx, job); !apierrors.IsInvalid(err) {
			t.Fatalf("Create() error = %v, want Invalid", err)
		}
	})

	t.Run("rejects immutable task update", func(t *testing.T) {
		job := validJob("immutable")
		if err := client.Create(ctx, job); err != nil {
			t.Fatal(err)
		}
		job.Spec.Task.Image = "docker.io/library/busybox:1.37"
		if err := client.Update(ctx, job); !apierrors.IsInvalid(err) {
			t.Fatalf("Update() error = %v, want Invalid", err)
		}
	})

	t.Run("allows one-way cancellation", func(t *testing.T) {
		job := validJob("cancel")
		if err := client.Create(ctx, job); err != nil {
			t.Fatal(err)
		}
		job.Spec.DesiredState = executionv1alpha1.JobDesiredStateCancelled
		if err := client.Update(ctx, job); err != nil {
			t.Fatalf("cancel update: %v", err)
		}
		job.Spec.DesiredState = executionv1alpha1.JobDesiredStateRunning
		if err := client.Update(ctx, job); !apierrors.IsInvalid(err) {
			t.Fatalf("reverse cancellation error = %v, want Invalid", err)
		}
	})
}

func validJob(name string) *executionv1alpha1.Job {
	//nolint:modernize // ObjectMeta is promoted through embedded TypeMeta; embedlit produces invalid Go here.
	return &executionv1alpha1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "api-validation"},
		Spec: executionv1alpha1.JobSpec{
			Task: executionv1alpha1.TaskSpec{Image: "docker.io/library/alpine:3.22"},
		},
	}
}
