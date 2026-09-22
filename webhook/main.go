// Command ingress-host-prefix-webhook is a Kubernetes mutating admission
// webhook. For every Ingress CREATE/UPDATE, it adds one extra spec.rules
// entry per existing rule that has a non-empty host, with the host prefixed
// by a configurable string (HOST_PREFIX env var). The extra rule copies the
// http (paths/backends) section of the original rule. Rules whose host is
// already prefixed are left alone, so repeated mutation of an
// already-mutated Ingress is a no-op (idempotent).
//
// For every original host that gets a prefixed twin added to spec.rules,
// the webhook also appends that same prefixed host to the hosts list of any
// spec.tls entry that already references the original host. This keeps the
// TLS hosts list in sync with the new rule even though the existing
// certificate Secret obviously won't actually cover the new hostname; no
// certificate validation is performed or implied.
package main

import (
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	admissionv1 "k8s.io/api/admission/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const defaultHostPrefix = "prefixed-"

// hostPrefix is read once at startup from HOST_PREFIX. Hot-reload is not
// supported; a pod restart is required to pick up a changed value.
var hostPrefix string

// admissionRequestsTotal counts every /mutate request handled, broken down
// by outcome:
//   - "mutated": the request was successfully parsed and produced a
//     non-empty JSON patch.
//   - "unmutated": the request was successfully parsed but required no
//     changes (already-prefixed hosts, no matching rules, etc.).
//   - "error": the request body/AdmissionReview/Ingress could not be
//     parsed, or the resulting patch could not be marshaled. The object is
//     still admitted unmutated (fail open); see handleMutate.
//
// Registered on the default Prometheus registry so it's exposed on
// /metrics alongside the automatically-registered process/go collectors.
var admissionRequestsTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "webhook_admission_requests_total",
		Help: "Total number of /mutate admission requests handled, by outcome (mutated, unmutated, error).",
	},
	[]string{"outcome"},
)

// buildInfo exposes the running binary's version as a label so it can be
// displayed/joined against in Grafana. The gauge value itself is always 1;
// the "version" label is what matters.
var buildInfo = prometheus.NewGaugeVec(
	prometheus.GaugeOpts{
		Name: "webhook_build_info",
		Help: "Build/version info for the running webhook binary. Value is always 1; see the version label.",
	},
	[]string{"version"},
)

func init() {
	prometheus.MustRegister(admissionRequestsTotal)
	prometheus.MustRegister(buildInfo)
}

// jsonPatchOp is a single RFC 6902 JSON Patch operation.
type jsonPatchOp struct {
	Op    string      `json:"op"`
	Path  string      `json:"path"`
	Value interface{} `json:"value,omitempty"`
}

func main() {
	hostPrefix = os.Getenv("HOST_PREFIX")
	if hostPrefix == "" {
		hostPrefix = defaultHostPrefix
	}

	certFile := getEnv("TLS_CERT_FILE", "/etc/webhook/tls/tls.crt")
	keyFile := getEnv("TLS_KEY_FILE", "/etc/webhook/tls/tls.key")
	addr := getEnv("LISTEN_ADDR", ":8443")
	metricsAddr := getEnv("METRICS_LISTEN_ADDR", ":8080")
	version := getEnv("APP_VERSION", "dev")

	buildInfo.WithLabelValues(version).Set(1)

	mux := http.NewServeMux()
	mux.HandleFunc("/mutate", handleMutate)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	// /metrics is served on a separate plain-HTTP listener rather than the
	// TLS admission listener above: Prometheus scraping the webhook's
	// self-signed admission cert would add unnecessary complexity (custom
	// CA trust, etc.) for an endpoint that carries no sensitive data.
	metricsMux := http.NewServeMux()
	metricsMux.Handle("/metrics", promhttp.Handler())

	go func() {
		log.Printf("starting metrics listener: addr=%s", metricsAddr)
		if err := http.ListenAndServe(metricsAddr, metricsMux); err != nil {
			log.Fatalf("metrics server error: %v", err)
		}
	}()

	log.Printf("starting ingress-host-prefix-webhook: hostPrefix=%q listenAddr=%s certFile=%s keyFile=%s version=%s",
		hostPrefix, addr, certFile, keyFile, version)

	server := &http.Server{
		Addr:    addr,
		Handler: mux,
		TLSConfig: &tls.Config{
			MinVersion: tls.VersionTLS12,
		},
	}
	if err := server.ListenAndServeTLS(certFile, keyFile); err != nil {
		log.Fatalf("server error: %v", err)
	}
}

func getEnv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func handleMutate(w http.ResponseWriter, r *http.Request) {
	if r.Body == nil {
		admissionRequestsTotal.WithLabelValues("error").Inc()
		http.Error(w, "empty request body", http.StatusBadRequest)
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		admissionRequestsTotal.WithLabelValues("error").Inc()
		http.Error(w, fmt.Sprintf("failed to read body: %v", err), http.StatusBadRequest)
		return
	}

	var review admissionv1.AdmissionReview
	if err := json.Unmarshal(body, &review); err != nil {
		admissionRequestsTotal.WithLabelValues("error").Inc()
		http.Error(w, fmt.Sprintf("failed to unmarshal AdmissionReview: %v", err), http.StatusBadRequest)
		return
	}
	if review.Request == nil {
		admissionRequestsTotal.WithLabelValues("error").Inc()
		http.Error(w, "AdmissionReview.request is nil", http.StatusBadRequest)
		return
	}

	req := review.Request
	resp := &admissionv1.AdmissionResponse{
		UID:     req.UID,
		Allowed: true,
	}

	outcome := "unmutated"
	patch, err := buildPatch(req.Object.Raw, hostPrefix)
	if err != nil {
		// Fail open: log and admit the object unmutated rather than blocking
		// the user's Ingress on a webhook bug.
		outcome = "error"
		log.Printf("uid=%s: failed to build patch, admitting unmutated: %v", req.UID, err)
	} else if len(patch) > 0 {
		patchBytes, merr := json.Marshal(patch)
		if merr != nil {
			outcome = "error"
			log.Printf("uid=%s: failed to marshal patch, admitting unmutated: %v", req.UID, merr)
		} else {
			pt := admissionv1.PatchTypeJSONPatch
			resp.Patch = patchBytes
			resp.PatchType = &pt
			outcome = "mutated"
			log.Printf("uid=%s: %s/%s: applied %d patch op(s) (rules+tls)", req.UID, req.Namespace, req.Name, len(patch))
		}
	}
	admissionRequestsTotal.WithLabelValues(outcome).Inc()

	respReview := admissionv1.AdmissionReview{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "admission.k8s.io/v1",
			Kind:       "AdmissionReview",
		},
		Response: resp,
	}

	out, err := json.Marshal(respReview)
	if err != nil {
		http.Error(w, fmt.Sprintf("failed to marshal response: %v", err), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(out)
}

// buildPatch inspects the raw Ingress object and returns a JSON Patch
// (RFC 6902) that appends one extra rule per existing rule whose host is
// non-empty and does not already start with the configured prefix. The new
// rule copies the http section (paths/backends) of the original rule.
//
// Because a new rule is only ever produced by copying an existing rule, the
// target array (spec.rules) is guaranteed non-empty whenever we emit an
// "add" op, so "/spec/rules/-" is always safe to use (no need to fall back
// to replacing a null array).
//
// For every original host that gets a prefixed twin added to spec.rules,
// buildPatch also scans spec.tls for entries whose hosts list already
// contains the original host, and appends the prefixed twin to that same
// entry's hosts list (via /spec/tls/<idx>/hosts/-). TLS entry indices are
// stable across the ops emitted here since we only ever append within an
// existing entry's hosts array, never insert/remove whole TLS entries.
func buildPatch(raw []byte, prefix string) ([]jsonPatchOp, error) {
	if len(raw) == 0 {
		return nil, nil
	}

	var ing networkingv1.Ingress
	if err := json.Unmarshal(raw, &ing); err != nil {
		return nil, fmt.Errorf("unmarshal ingress: %w", err)
	}

	return mutateIngress(&ing, prefix), nil
}

// mutateIngress is the pure core of buildPatch: given an already-decoded
// Ingress and the configured host prefix, it returns the JSON Patch ops to
// apply. It performs no I/O and has no dependency on package-level state,
// which makes it directly unit-testable without going through the HTTP
// handler or JSON (de)serialization of the Ingress itself.
func mutateIngress(ing *networkingv1.Ingress, prefix string) []jsonPatchOp {
	// Build the set of hosts already present in spec.rules once per request,
	// so we can tell whether a prefixed twin for a given host already
	// exists elsewhere in the list (not just whether the rule we're
	// currently looking at is itself prefixed). Without this, a no-op
	// UPDATE on an already-mutated Ingress would re-scan the original
	// (unprefixed) rules and append duplicate prefixed twins forever.
	existingHosts := make(map[string]bool, len(ing.Spec.Rules))
	for _, rule := range ing.Spec.Rules {
		if rule.Host != "" {
			existingHosts[rule.Host] = true
		}
	}

	// Build a per-TLS-entry host set (index -> hosts already listed on that
	// entry). Used both to test whether an entry references a given
	// original host, and to keep the "append prefixed twin to tls.hosts"
	// step idempotent across repeated admissions of the same object.
	tlsHostSets := make([]map[string]bool, len(ing.Spec.TLS))
	for i, t := range ing.Spec.TLS {
		set := make(map[string]bool, len(t.Hosts))
		for _, h := range t.Hosts {
			set[h] = true
		}
		tlsHostSets[i] = set
	}

	var patch []jsonPatchOp
	for _, rule := range ing.Spec.Rules {
		if rule.Host == "" {
			// Skip wildcard/default-backend rules with no host.
			continue
		}
		if strings.HasPrefix(rule.Host, prefix) {
			// Already mutated (or user already used the prefix); skip to
			// stay idempotent across repeated CREATE/UPDATE admissions.
			continue
		}

		newHost := prefix + rule.Host
		if existingHosts[newHost] {
			// A prefixed twin for this host already exists in spec.rules
			// (added by a prior admission), so adding another would
			// duplicate it. Skip to stay idempotent. This also means we
			// skip the TLS mirroring below, since a prior admission would
			// already have applied it too.
			continue
		}

		newRule := networkingv1.IngressRule{
			Host:             newHost,
			IngressRuleValue: rule.IngressRuleValue,
		}
		patch = append(patch, jsonPatchOp{
			Op:    "add",
			Path:  "/spec/rules/-",
			Value: newRule,
		})
		// Record the newly-added host too, in case a later JSON Patch "add"
		// op in this same batch would otherwise produce the same twin
		// again (e.g. duplicate host entries in the incoming spec).
		existingHosts[newHost] = true

		// Mirror the new prefixed host into any TLS entry that already
		// lists the original host. The underlying certificate in that
		// entry's Secret obviously won't actually cover the new hostname;
		// we only keep the hosts list entries in sync, with no certificate
		// validation performed or implied.
		for i := range ing.Spec.TLS {
			set := tlsHostSets[i]
			if !set[rule.Host] {
				continue // this TLS entry doesn't reference the original host
			}
			if set[newHost] {
				continue // already present (e.g. from a prior admission); stay idempotent
			}
			patch = append(patch, jsonPatchOp{
				Op:    "add",
				Path:  fmt.Sprintf("/spec/tls/%d/hosts/-", i),
				Value: newHost,
			})
			set[newHost] = true
		}
	}

	return patch
}
