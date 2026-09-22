package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	admissionv1 "k8s.io/api/admission/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

const testPrefix = "prefixed-"

// httpValue builds a minimal, distinguishable IngressRuleValue so tests can
// assert it was copied verbatim onto the new twin rule.
func httpValue(pathSuffix string) networkingv1.IngressRuleValue {
	pt := networkingv1.PathTypePrefix
	return networkingv1.IngressRuleValue{
		HTTP: &networkingv1.HTTPIngressRuleValue{
			Paths: []networkingv1.HTTPIngressPath{
				{
					Path:     "/" + pathSuffix,
					PathType: &pt,
					Backend: networkingv1.IngressBackend{
						Service: &networkingv1.IngressServiceBackend{
							Name: "svc-" + pathSuffix,
							Port: networkingv1.ServiceBackendPort{Number: 80},
						},
					},
				},
			},
		},
	}
}

func rule(host, pathSuffix string) networkingv1.IngressRule {
	return networkingv1.IngressRule{
		Host:             host,
		IngressRuleValue: httpValue(pathSuffix),
	}
}

func ingressWithRules(rules ...networkingv1.IngressRule) *networkingv1.Ingress {
	return &networkingv1.Ingress{
		Spec: networkingv1.IngressSpec{
			Rules: rules,
		},
	}
}

// countOps returns how many patch ops match op+path exactly, and how many
// "add /spec/rules/-" ops exist whose Value is an IngressRule with the given
// host.
func addRuleOpForHost(t *testing.T, patch []jsonPatchOp, host string) (jsonPatchOp, bool) {
	t.Helper()
	for _, p := range patch {
		if p.Op != "add" || p.Path != "/spec/rules/-" {
			continue
		}
		nr, ok := p.Value.(networkingv1.IngressRule)
		if !ok {
			t.Fatalf("patch op Value is not an IngressRule: %#v", p.Value)
		}
		if nr.Host == host {
			return p, true
		}
	}
	return jsonPatchOp{}, false
}

func tlsAddOpsForPath(patch []jsonPatchOp, path string) []jsonPatchOp {
	var out []jsonPatchOp
	for _, p := range patch {
		if p.Op == "add" && p.Path == path {
			out = append(out, p)
		}
	}
	return out
}

// 1. Basic rule mutation: single rule, no existing twin -> one new rule added
// with host = prefix + originalHost and the same http paths/backend copied.
func TestMutateIngress_BasicRuleMutation(t *testing.T) {
	ing := ingressWithRules(rule("example.com", "a"))

	patch := mutateIngress(ing, testPrefix)

	if len(patch) != 1 {
		t.Fatalf("expected exactly 1 patch op, got %d: %#v", len(patch), patch)
	}

	op, found := addRuleOpForHost(t, patch, "prefixed-example.com")
	if !found {
		t.Fatalf("expected an add-rule op for host %q, got %#v", "prefixed-example.com", patch)
	}
	if op.Path != "/spec/rules/-" {
		t.Errorf("expected path /spec/rules/-, got %q", op.Path)
	}

	newRule := op.Value.(networkingv1.IngressRule)
	origRule := ing.Spec.Rules[0]
	if newRule.Host != testPrefix+origRule.Host {
		t.Errorf("new rule host = %q, want %q", newRule.Host, testPrefix+origRule.Host)
	}
	if newRule.IngressRuleValue.HTTP == nil || origRule.IngressRuleValue.HTTP == nil {
		t.Fatalf("expected HTTP value on both rules")
	}
	if newRule.IngressRuleValue.HTTP.Paths[0].Path != origRule.IngressRuleValue.HTTP.Paths[0].Path {
		t.Errorf("new rule path = %q, want copy of original %q",
			newRule.IngressRuleValue.HTTP.Paths[0].Path, origRule.IngressRuleValue.HTTP.Paths[0].Path)
	}
	if newRule.IngressRuleValue.HTTP.Paths[0].Backend.Service.Name != origRule.IngressRuleValue.HTTP.Paths[0].Backend.Service.Name {
		t.Errorf("new rule backend service = %q, want copy of original %q",
			newRule.IngressRuleValue.HTTP.Paths[0].Backend.Service.Name, origRule.IngressRuleValue.HTTP.Paths[0].Backend.Service.Name)
	}
}

// 2. Multiple hosts in one Ingress: each rule gets its own twin.
func TestMutateIngress_MultipleHosts(t *testing.T) {
	ing := ingressWithRules(
		rule("a.example.com", "a"),
		rule("b.example.com", "b"),
		rule("c.example.com", "c"),
	)

	patch := mutateIngress(ing, testPrefix)

	if len(patch) != 3 {
		t.Fatalf("expected exactly 3 patch ops, got %d: %#v", len(patch), patch)
	}
	for _, host := range []string{"a.example.com", "b.example.com", "c.example.com"} {
		wantHost := testPrefix + host
		if _, found := addRuleOpForHost(t, patch, wantHost); !found {
			t.Errorf("expected a twin rule for host %q, none found in patch", wantHost)
		}
	}
}

// 3. Skip already-prefixed hosts: a rule whose host already starts with the
// prefix gets no twin (must not be double-prefixed).
func TestMutateIngress_SkipAlreadyPrefixedHost(t *testing.T) {
	ing := ingressWithRules(rule("prefixed-example.com", "a"))

	patch := mutateIngress(ing, testPrefix)

	if len(patch) != 0 {
		t.Fatalf("expected no patch ops for an already-prefixed host, got %d: %#v", len(patch), patch)
	}
}

// 4. Rules idempotency regression test: an Ingress that already contains
// both the original host rule and its prefixed twin must not get another
// patch op emitted for that pair.
func TestMutateIngress_RulesIdempotency(t *testing.T) {
	ing := ingressWithRules(
		rule("example.com", "a"),
		rule("prefixed-example.com", "a"),
	)

	patch := mutateIngress(ing, testPrefix)

	if len(patch) != 0 {
		t.Fatalf("expected no patch ops when the prefixed twin already exists, got %d: %#v", len(patch), patch)
	}
}

// 5. TLS mirroring: a TLS entry whose hosts include an original host that IS
// getting a rule twin added gets an "add" op appending the prefixed host to
// that TLS entry's hosts array at the correct index.
func TestMutateIngress_TLSMirroring(t *testing.T) {
	ing := ingressWithRules(rule("example.com", "a"))
	ing.Spec.TLS = []networkingv1.IngressTLS{
		{Hosts: []string{"example.com"}, SecretName: "example-tls"},
	}

	patch := mutateIngress(ing, testPrefix)

	if _, found := addRuleOpForHost(t, patch, "prefixed-example.com"); !found {
		t.Fatalf("expected a rule twin op, patch=%#v", patch)
	}

	tlsOps := tlsAddOpsForPath(patch, "/spec/tls/0/hosts/-")
	if len(tlsOps) != 1 {
		t.Fatalf("expected exactly 1 tls add op at /spec/tls/0/hosts/-, got %d: %#v", len(tlsOps), patch)
	}
	if tlsOps[0].Value != "prefixed-example.com" {
		t.Errorf("tls add op value = %v, want %q", tlsOps[0].Value, "prefixed-example.com")
	}
}

// 6. TLS asymmetry: two rule hosts, only one of which appears in any TLS
// entry -> the TLS patch op is only emitted for the one with a matching TLS
// entry.
func TestMutateIngress_TLSAsymmetry(t *testing.T) {
	ing := ingressWithRules(
		rule("secure.example.com", "a"),
		rule("plain.example.com", "b"),
	)
	ing.Spec.TLS = []networkingv1.IngressTLS{
		{Hosts: []string{"secure.example.com"}, SecretName: "secure-tls"},
	}

	patch := mutateIngress(ing, testPrefix)

	// Both hosts should still get rule twins.
	if _, found := addRuleOpForHost(t, patch, "prefixed-secure.example.com"); !found {
		t.Errorf("expected rule twin for secure.example.com")
	}
	if _, found := addRuleOpForHost(t, patch, "prefixed-plain.example.com"); !found {
		t.Errorf("expected rule twin for plain.example.com")
	}

	// Only one TLS add op total, and only for index 0 (the entry referencing
	// secure.example.com).
	var tlsOps []jsonPatchOp
	for _, p := range patch {
		if p.Op == "add" && p.Path == "/spec/tls/0/hosts/-" {
			tlsOps = append(tlsOps, p)
		}
	}
	if len(tlsOps) != 1 {
		t.Fatalf("expected exactly 1 tls add op for the TLS entry, got %d: %#v", len(tlsOps), patch)
	}
	if tlsOps[0].Value != "prefixed-secure.example.com" {
		t.Errorf("tls add op value = %v, want %q", tlsOps[0].Value, "prefixed-secure.example.com")
	}

	for _, p := range patch {
		if p.Op == "add" && p.Value == "prefixed-plain.example.com" {
			t.Errorf("did not expect a tls add op for plain.example.com (no matching TLS entry), got %#v", p)
		}
	}
}

// 7. TLS idempotency regression test: a TLS entry whose hosts already
// contains both the original and prefixed host must get no new patch op.
func TestMutateIngress_TLSIdempotency(t *testing.T) {
	ing := ingressWithRules(
		rule("example.com", "a"),
		rule("prefixed-example.com", "a"),
	)
	ing.Spec.TLS = []networkingv1.IngressTLS{
		{Hosts: []string{"example.com", "prefixed-example.com"}, SecretName: "example-tls"},
	}

	patch := mutateIngress(ing, testPrefix)

	if len(patch) != 0 {
		t.Fatalf("expected no patch ops (rules and tls both already mutated), got %d: %#v", len(patch), patch)
	}
}

// 8. No TLS block at all: rules still mutate normally, no panic, no
// TLS-related patch ops.
func TestMutateIngress_NoTLSBlock(t *testing.T) {
	ing := ingressWithRules(rule("example.com", "a"))
	ing.Spec.TLS = nil

	patch := mutateIngress(ing, testPrefix)

	if len(patch) != 1 {
		t.Fatalf("expected exactly 1 patch op (rule twin only), got %d: %#v", len(patch), patch)
	}
	for _, p := range patch {
		if p.Op == "add" && len(p.Path) >= len("/spec/tls") && p.Path[:len("/spec/tls")] == "/spec/tls" {
			t.Errorf("did not expect any tls patch op when spec.tls is nil, got %#v", p)
		}
	}
}

// 9. Empty/missing host on a rule (e.g. a default-backend-only rule) is
// skipped: no patch op, no panic.
func TestMutateIngress_EmptyHostSkipped(t *testing.T) {
	ing := ingressWithRules(
		rule("", "default"),
		rule("example.com", "a"),
	)

	patch := mutateIngress(ing, testPrefix)

	if len(patch) != 1 {
		t.Fatalf("expected exactly 1 patch op (only for the non-empty host), got %d: %#v", len(patch), patch)
	}
	if _, found := addRuleOpForHost(t, patch, ""); found {
		t.Errorf("did not expect a twin rule for the empty host")
	}
	if _, found := addRuleOpForHost(t, patch, "prefixed-example.com"); !found {
		t.Errorf("expected a twin rule for example.com")
	}
}

// EmptyHostOnly is a corner case of #9: an Ingress with only a
// default-backend rule (empty host, possibly no TLS) produces no ops and
// does not panic.
func TestMutateIngress_EmptyHostOnly_NoPanic(t *testing.T) {
	ing := ingressWithRules(rule("", "default"))

	patch := mutateIngress(ing, testPrefix)

	if len(patch) != 0 {
		t.Fatalf("expected no patch ops for a host-less rule, got %d: %#v", len(patch), patch)
	}
}

// --- Prometheus metrics ---

// TestMetrics_Registered confirms admissionRequestsTotal and buildInfo are
// registered on the default registry (the one promhttp.Handler() and
// prometheus.MustRegister in init() both use), so /metrics actually exposes
// them alongside the auto-registered process/go collectors. A CounterVec/
// GaugeVec only emits a sample once a specific label combination has been
// materialized (e.g. via WithLabelValues), so a distinct probe label is
// touched first purely to force that materialization for this assertion;
// it doesn't affect other tests' deltas since no other test uses this
// label value.
func TestMetrics_Registered(t *testing.T) {
	admissionRequestsTotal.WithLabelValues("test_registration_probe").Add(0)
	buildInfo.WithLabelValues("test_registration_probe").Set(0)

	names := []string{"webhook_admission_requests_total", "webhook_build_info"}
	for _, name := range names {
		count, err := testutil.GatherAndCount(prometheus.DefaultGatherer, name)
		if err != nil {
			t.Fatalf("GatherAndCount(%q) error: %v", name, err)
		}
		if count == 0 {
			t.Errorf("expected metric %q to be registered on the default registry, found 0 series", name)
		}
	}
}

// TestBuildInfo_SetWithVersionLabel confirms the build-info gauge exposes
// the version as a label (value fixed at 1), matching what main() does at
// startup with APP_VERSION.
func TestBuildInfo_SetWithVersionLabel(t *testing.T) {
	const version = "test-version-1.2.3"
	buildInfo.WithLabelValues(version).Set(1)

	got := testutil.ToFloat64(buildInfo.WithLabelValues(version))
	if got != 1 {
		t.Errorf("buildInfo{version=%q} = %v, want 1", version, got)
	}
}

// TestHandleMutate_MetricsOutcomes drives handleMutate directly (bypassing
// the TLS listener, which isn't needed for handler-level testing) with
// requests that exercise each outcome branch, and asserts
// admissionRequestsTotal's per-label counter increments by exactly 1 each
// time. Deltas are used (rather than asserting an absolute value) because
// admissionRequestsTotal is process-global shared state across subtests.
func TestHandleMutate_MetricsOutcomes(t *testing.T) {
	// handleMutate reads the package-level hostPrefix var (normally set once
	// in main() from HOST_PREFIX), which is still its zero value "" here
	// since main() never runs in tests. Set/restore it so buildPatch behaves
	// the same way it does in production instead of treating every host as
	// already-prefixed (strings.HasPrefix(host, "") is always true).
	prevPrefix := hostPrefix
	hostPrefix = testPrefix
	t.Cleanup(func() { hostPrefix = prevPrefix })

	post := func(t *testing.T, body []byte) *httptest.ResponseRecorder {
		t.Helper()
		var r *http.Request
		if body == nil {
			r = httptest.NewRequest(http.MethodPost, "/mutate", nil)
			r.Body = nil
		} else {
			r = httptest.NewRequest(http.MethodPost, "/mutate", bytes.NewReader(body))
		}
		w := httptest.NewRecorder()
		handleMutate(w, r)
		return w
	}

	delta := func(t *testing.T, outcome string, fn func()) {
		t.Helper()
		before := testutil.ToFloat64(admissionRequestsTotal.WithLabelValues(outcome))
		fn()
		after := testutil.ToFloat64(admissionRequestsTotal.WithLabelValues(outcome))
		if after-before != 1 {
			t.Errorf("outcome=%q counter delta = %v, want 1", outcome, after-before)
		}
	}

	t.Run("error_nilBody", func(t *testing.T) {
		delta(t, "error", func() { post(t, nil) })
	})

	t.Run("error_invalidJSON", func(t *testing.T) {
		delta(t, "error", func() { post(t, []byte("not json")) })
	})

	t.Run("error_nilRequest", func(t *testing.T) {
		review := admissionv1.AdmissionReview{}
		body, err := json.Marshal(review)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		delta(t, "error", func() { post(t, body) })
	})

	t.Run("unmutated_emptyHost", func(t *testing.T) {
		ing := ingressWithRules(rule("", "default"))
		raw, err := json.Marshal(ing)
		if err != nil {
			t.Fatalf("marshal ingress: %v", err)
		}
		review := admissionv1.AdmissionReview{
			Request: &admissionv1.AdmissionRequest{
				UID:    "test-uid-unmutated",
				Object: runtime.RawExtension{Raw: raw},
			},
		}
		body, err := json.Marshal(review)
		if err != nil {
			t.Fatalf("marshal review: %v", err)
		}
		delta(t, "unmutated", func() { post(t, body) })
	})

	t.Run("mutated_basicRule", func(t *testing.T) {
		ing := ingressWithRules(rule("metrics-example.com", "a"))
		raw, err := json.Marshal(ing)
		if err != nil {
			t.Fatalf("marshal ingress: %v", err)
		}
		review := admissionv1.AdmissionReview{
			Request: &admissionv1.AdmissionRequest{
				UID:    "test-uid-mutated",
				Object: runtime.RawExtension{Raw: raw},
			},
		}
		body, err := json.Marshal(review)
		if err != nil {
			t.Fatalf("marshal review: %v", err)
		}
		delta(t, "mutated", func() { post(t, body) })
	})
}
