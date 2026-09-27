package nuts

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/caddyserver/caddy/v2"
	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/zap"
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

// TestFuzzWorkflowRunsEveryTarget keeps the nightly fuzz workflow's matrix
// in step with the package's Fuzz functions, so a new target does not end up
// running only against its seeds.
func TestFuzzWorkflowRunsEveryTarget(t *testing.T) {
	workflow, err := os.ReadFile(filepath.Join(".github", "workflows", "fuzz.yml"))
	if err != nil {
		t.Fatalf("read fuzz.yml: %v", err)
	}
	inMatrix := map[string]bool{}
	for _, m := range regexp.MustCompile(`(?m)^\s+- (Fuzz\w+)\s*$`).FindAllStringSubmatch(string(workflow), -1) {
		inMatrix[m[1]] = true
	}
	files, err := filepath.Glob("*_test.go")
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	defined := map[string]bool{}
	for _, file := range files {
		src, err := os.ReadFile(file)
		if err != nil {
			t.Fatalf("read %s: %v", file, err)
		}
		for _, m := range regexp.MustCompile(`(?m)^func (Fuzz\w+)\(f \*testing\.F\)`).FindAllStringSubmatch(string(src), -1) {
			defined[m[1]] = true
		}
	}
	if len(defined) == 0 {
		t.Fatal("found no Fuzz functions; the pattern is out of date")
	}
	for name := range defined {
		if !inMatrix[name] {
			t.Errorf("%s is missing from the matrix in .github/workflows/fuzz.yml", name)
		}
	}
	for name := range inMatrix {
		if !defined[name] {
			t.Errorf(".github/workflows/fuzz.yml runs %s, which no test file defines", name)
		}
	}
}

// TestAgentsFileMapListsEveryGoFile keeps the file map in AGENTS.md from
// drifting when source or test files are added, split or renamed.
func TestAgentsFileMapListsEveryGoFile(t *testing.T) {
	agents, err := os.ReadFile("AGENTS.md")
	if err != nil {
		t.Fatalf("read AGENTS.md: %v", err)
	}
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	for _, file := range files {
		if !strings.Contains(string(agents), "["+file+"]("+file+")") {
			t.Errorf("AGENTS.md does not list %s", file)
		}
	}
}

// TestDocumentedDefaultsMatchProvision keeps the Default column of
// docs/CONFIGURATION.md in step with what Provision applies. Provision
// normalises every optional setting before it dials NATS, so a server that
// refuses the connection still leaves the defaults in place to compare.
func TestDocumentedDefaultsMatchProvision(t *testing.T) {
	doc, err := os.ReadFile(filepath.Join("docs", "CONFIGURATION.md"))
	if err != nil {
		t.Fatalf("read CONFIGURATION.md: %v", err)
	}
	h := &Handler{NatsURL: "nats://127.0.0.1:1", StreamName: "EVENTS", logger: zap.NewNop()}
	if err := h.Provision(caddy.Context{Context: context.Background()}); err == nil {
		t.Fatal("Provision connected to nats://127.0.0.1:1")
	}
	raw, err := json.Marshal(h)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	// Absent (omitempty), false, zero and empty all read as "".
	normalise := func(s string) string {
		if s == "false" || s == "0" {
			return ""
		}
		return s
	}
	describe := func(v any) string {
		switch v := v.(type) {
		case float64:
			return strconv.FormatFloat(v, 'f', -1, 64)
		case string:
			return v
		case bool:
			return strconv.FormatBool(v)
		case []any:
			parts := make([]string, len(v))
			for i, item := range v {
				parts[i] = fmt.Sprint(item)
			}
			return strings.Join(parts, " ")
		}
		return ""
	}
	firstCode := regexp.MustCompile("`([^`]*)`")
	checked, inTable := 0, false
	for _, line := range strings.Split(string(doc), "\n") {
		if strings.HasPrefix(line, "| Caddyfile directive | JSON field | Default |") {
			inTable = true
			continue
		}
		if !strings.HasPrefix(line, "|") {
			inTable = false
		}
		if !inTable || strings.HasPrefix(line, "| ---") {
			continue
		}
		cells := strings.Split(line, "|")
		if len(cells) < 4 {
			t.Fatalf("malformed row: %s", line)
		}
		field := strings.Trim(strings.TrimSpace(cells[2]), "`")
		documented := strings.TrimSpace(cells[3])
		want := ""
		switch documented {
		case "Required":
			continue
		case "Empty":
		default:
			m := firstCode.FindStringSubmatch(documented)
			if m == nil {
				t.Errorf("%s: cannot read the documented default %q", field, documented)
				continue
			}
			want = m[1]
		}
		if actual := describe(got[field]); normalise(actual) != normalise(want) {
			t.Errorf("%s: CONFIGURATION.md says the default is %q, Provision leaves %q", field, want, actual)
		}
		checked++
	}
	if checked < 25 {
		t.Fatalf("checked only %d defaults; the table format changed", checked)
	}
}

// TestDocumentedStreamWatchTimings: TROUBLESHOOTING.md tells operators how
// soon NUTS notices a recreated or rewound stream (#133), so its numbers must
// follow streamWatchInterval.
func TestDocumentedStreamWatchTimings(t *testing.T) {
	doc, err := os.ReadFile(filepath.Join("docs", "TROUBLESHOOTING.md"))
	if err != nil {
		t.Fatalf("read TROUBLESHOOTING.md: %v", err)
	}
	text := strings.Join(strings.Fields(string(doc)), " ")
	for _, want := range []string{
		fmt.Sprintf("every %d seconds, and closes the SSE streams", int(streamWatchInterval/time.Second)),
		fmt.Sprintf("a read at least %d seconds later still finds it back", int(streamWatchInterval/2/time.Second)),
	} {
		if !strings.Contains(text, want) {
			t.Errorf("TROUBLESHOOTING.md does not say %q", want)
		}
	}
}
