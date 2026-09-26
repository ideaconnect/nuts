package nuts

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
)

// metricReference matches the metric names NUTS registers: every counter ends
// in _total, and the gauges are listed by name. Other nuts_ names (consumer
// names, cookie names) do not match. A new gauge must be added here, or
// TestRegisteredMetricsAreDocumented reports it as undocumented.
var metricReference = regexp.MustCompile(`\bnuts_(?:[a-z_]+_total|active_connections|shared_subscriptions)\b`)

// registeredMetrics reads the metric names out of metrics.go.
func registeredMetrics(t *testing.T) map[string]bool {
	t.Helper()
	src, err := os.ReadFile("metrics.go")
	if err != nil {
		t.Fatalf("read metrics.go: %v", err)
	}
	names := map[string]bool{}
	for _, m := range regexp.MustCompile(`Name:\s+"([a-z_]+)"`).FindAllStringSubmatch(string(src), -1) {
		names["nuts_"+m[1]] = true
	}
	if len(names) < 10 {
		t.Fatalf("found only %d metric names in metrics.go; the pattern is out of date", len(names))
	}
	return names
}

// TestMetricNamesInDocsAndOpsExist keeps the documentation, alert rules and
// dashboard from referring to metrics NUTS does not register, for example
// after a rename.
func TestMetricNamesInDocsAndOpsExist(t *testing.T) {
	registered := registeredMetrics(t)
	files := []string{"README.md", "DOCKERHUB_README.md", "ops/prometheus-alerts.yml", "ops/grafana-dashboard.json"}
	for _, pattern := range []string{"docs/*.md", "website/docs/*.md"} {
		matches, err := filepath.Glob(pattern)
		if err != nil {
			t.Fatalf("glob %s: %v", pattern, err)
		}
		files = append(files, matches...)
	}
	for _, file := range files {
		if file == "docs/PLAN.md" {
			continue // the plan quotes issue text, including old names
		}
		content, err := os.ReadFile(file)
		if err != nil {
			t.Fatalf("read %s: %v", file, err)
		}
		for _, name := range metricReference.FindAllString(string(content), -1) {
			if !registered[name] {
				t.Errorf("%s mentions %s, which metrics.go does not register", file, name)
			}
		}
	}
}

// TestRegisteredMetricsAreDocumented: every metric appears in the README and
// website metric references, so operators can find what it means.
func TestRegisteredMetricsAreDocumented(t *testing.T) {
	registered := registeredMetrics(t)
	for _, file := range []string{"README.md", "website/docs/usage.md"} {
		content, err := os.ReadFile(file)
		if err != nil {
			t.Fatalf("read %s: %v", file, err)
		}
		documented := map[string]bool{}
		for _, name := range metricReference.FindAllString(string(content), -1) {
			documented[name] = true
		}
		var missing []string
		for name := range registered {
			if !documented[name] {
				missing = append(missing, name)
			}
		}
		sort.Strings(missing)
		if len(missing) > 0 {
			t.Errorf("%s does not document %v", file, missing)
		}
	}
}

// TestNutsCollectorsCoverEveryMetric: every metric defined in metrics.go
// must be in nutsCollectors, or Caddy's metrics endpoints never expose it.
func TestNutsCollectorsCoverEveryMetric(t *testing.T) {
	listed := map[string]bool{}
	for _, collector := range nutsCollectors() {
		descs := make(chan *prometheus.Desc, 4)
		collector.Describe(descs)
		close(descs)
		for desc := range descs {
			name := regexp.MustCompile(`fqName: "([a-z_]+)"`).FindStringSubmatch(desc.String())
			if name == nil {
				t.Fatalf("cannot read the name of %s", desc)
			}
			listed[name[1]] = true
		}
	}
	for name := range registeredMetrics(t) {
		if !listed[name] {
			t.Errorf("%s is defined in metrics.go but missing from nutsCollectors", name)
		}
	}
}

func TestRegisterMetrics(t *testing.T) {
	registry := prometheus.NewPedanticRegistry()
	for i := 0; i < 2; i++ { // a second nuts handler in the same config
		if err := registerMetrics(registry); err != nil {
			t.Fatalf("registerMetrics (call %d): %v", i+1, err)
		}
	}
	families, err := registry.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	found := false
	for _, family := range families {
		if family.GetName() == "nuts_active_connections" {
			found = true
		}
	}
	if !found {
		t.Fatal("nuts_active_connections missing from the registry")
	}
	if err := registerMetrics(nil); err != nil {
		t.Fatalf("registerMetrics(nil) = %v, want no-op", err)
	}
}
