package resources

import (
	"context"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// JobsManagerEnv reads the jobs-manager Deployment's first-container literal env
// into a map (valueFrom entries have no literal value and are skipped). It is
// release-scoped: it looks up the chart's standard "<release>-jobs-manager"
// name, falling back to a bare "jobs-manager" only when that deployment's
// app.kubernetes.io/instance label ties it to this release — never a different
// release's component (the same attribution rule doctor.findDeployment uses).
//
// Best-effort by design: an unreadable/absent deployment returns an empty map,
// and ParseTraining then reports the chart-default ceiling rather than failing —
// `resources show` is a read-only view, not a gate.
func JobsManagerEnv(ctx context.Context, cs kubernetes.Interface, ns, releaseName string) map[string]string {
	env := map[string]string{}
	dep := jobsManagerDeployment(ctx, cs, ns, releaseName)
	if dep == nil || len(dep.Spec.Template.Spec.Containers) == 0 {
		return env
	}
	for _, e := range dep.Spec.Template.Spec.Containers[0].Env {
		if e.Value != "" {
			env[e.Name] = e.Value
		}
	}
	return env
}

// jobsManagerDeployment resolves the release's jobs-manager Deployment, or nil.
// It mirrors doctor.findDeployment's attribution rule exactly, so a best-effort
// resources read can never be satisfied by a DIFFERENT release's component:
//
//	Release known   — take the chart's "<release>-jobs-manager", or a bare
//	                  "jobs-manager" ONLY when its app.kubernetes.io/instance
//	                  label ties it to this release. A deployment belonging to
//	                  another release is never accepted; missing → nil.
//	Release unknown — match any "<x>-jobs-manager"/"jobs-manager" by name, but
//	                  only when EXACTLY ONE exists. With several (multiple
//	                  releases, which discovery refuses to disambiguate) there is
//	                  no safe attribution, so return nil rather than guess and
//	                  read the wrong release's ceiling.
func jobsManagerDeployment(ctx context.Context, cs kubernetes.Interface, ns, releaseName string) *appsv1.Deployment {
	const suffix = "jobs-manager"
	if releaseName != "" {
		if d, err := cs.AppsV1().Deployments(ns).Get(ctx, releaseName+"-"+suffix, metav1.GetOptions{}); err == nil {
			return d
		}
		if d, err := cs.AppsV1().Deployments(ns).Get(ctx, suffix, metav1.GetOptions{}); err == nil &&
			d.Labels["app.kubernetes.io/instance"] == releaseName {
			return d
		}
		return nil
	}
	// Release unknown: match by name suffix, accepting the deployment only when
	// it's the unique carrier — otherwise there's no safe attribution.
	deps, err := cs.AppsV1().Deployments(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil
	}
	var match *appsv1.Deployment
	for i := range deps.Items {
		if n := deps.Items[i].Name; n == suffix || strings.HasSuffix(n, "-"+suffix) {
			if match != nil {
				return nil // ambiguous across releases — don't guess
			}
			match = &deps.Items[i]
		}
	}
	return match
}
