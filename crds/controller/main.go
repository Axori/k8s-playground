package main

import (
	"context"
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
)

var websiteGVK = schema.GroupVersionKind{
	Group:   "demo.com",
	Version: "v1",
	Kind:    "Website",
}

type WebsiteReconciler struct {
	client.Client
}

// Reconcile is the WHOLE controller. Called for one Website at a time, every time
// that Website (or a Deployment it owns) changes. Job: make the world match the
// spec. Must be IDEMPOTENT — calling it 10x == calling it 1x.
func (r *WebsiteReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	l := log.FromContext(ctx)

	// 1. OBSERVE — read desired state (the Website CR) from the cache.
	web := &unstructured.Unstructured{}
	web.SetGroupVersionKind(websiteGVK)
	if err := r.Get(ctx, req.NamespacedName, web); err != nil {
		// NotFound = the Website was deleted. Owner references (set below) let
		// Kubernetes garbage-collect the Deployment for us, so just stop.
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Pull fields out of the "just data" map.
	image, _, _ := unstructured.NestedString(web.Object, "spec", "image")
	replicas, _, _ := unstructured.NestedInt64(web.Object, "spec", "replicas")
	repl := int32(replicas)

	// 2. DECIDE + 3. ACT — create-or-update the Deployment to match.
	// CreateOrUpdate fetches the object; if absent it runs the mutate fn then
	// creates; if present it runs the mutate fn then updates only if changed.
	// This is what makes the loop idempotent and level-triggered.
	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: web.GetName(), Namespace: web.GetNamespace()},
	}
	op, err := controllerutil.CreateOrUpdate(ctx, r.Client, dep, func() error {
		labels := map[string]string{"website": web.GetName()}
		dep.Spec.Replicas = &repl
		dep.Spec.Selector = &metav1.LabelSelector{MatchLabels: labels}
		dep.Spec.Template = corev1.PodTemplateSpec{
			ObjectMeta: metav1.ObjectMeta{Labels: labels},
			Spec: corev1.PodSpec{
				Containers: []corev1.Container{{
					Name:  "web",
					Image: image,
					Ports: []corev1.ContainerPort{{ContainerPort: 80}},
				}},
			},
		}
		// Tie the Deployment's life to the Website: delete Website -> GC Deployment.
		// Also what makes `.Owns(...)` below re-trigger us on Deployment changes.
		return controllerutil.SetControllerReference(web, dep, r.Scheme())
	})
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("reconciling deployment for %s: %w", web.GetName(), err)
	}

	l.Info("reconciled", "website", web.GetName(), "op", op, "image", image, "replicas", repl)
	return ctrl.Result{}, nil
}

func main() {
	ctrl.SetLogger(zap.New(zap.UseDevMode(true)))

	// The Manager wires up the shared cache, clients, and leader election.
	// GetConfigOrDie() reads your kubeconfig (orbstack context) — so the
	// controller runs OUTSIDE the cluster as your admin user. No RBAC needed.
	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{})
	if err != nil {
		panic(err)
	}

	web := &unstructured.Unstructured{}
	web.SetGroupVersionKind(websiteGVK)

	// "For Websites, reconcile; also watch the Deployments I Own and re-reconcile
	// the parent Website when one changes." That second half = self-healing.
	if err := ctrl.NewControllerManagedBy(mgr).
		For(web).
		Owns(&appsv1.Deployment{}).
		Complete(&WebsiteReconciler{Client: mgr.GetClient()}); err != nil {
		panic(err)
	}

	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		panic(err)
	}
}
