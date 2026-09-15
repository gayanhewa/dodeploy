package host

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/gayanhewa/dodeploy/internal/appspec"
)

// sampleOutput mirrors what metricsScript prints on an Ubuntu host.
const sampleOutput = `cpu_cores=2
cpu_usage=37.5
mem_total_kb=2013265
mem_available_kb=1509949
swap_total_kb=0
swap_free_kb=0
load1=0.42
load5=0.31
load15=0.22
uptime_seconds=90061
disk / 41152736 8230547 30824861
disk /srv/apps 41152736 8230547 30824861
some_future_key=ignored
`

func TestParseMetrics(t *testing.T) {
	m, err := ParseMetrics(sampleOutput)
	if err != nil {
		t.Fatalf("ParseMetrics: %v", err)
	}
	if m.Cores != 2 {
		t.Errorf("Cores = %d, want 2", m.Cores)
	}
	if m.CPUPercent != 37.5 {
		t.Errorf("CPUPercent = %v, want 37.5", m.CPUPercent)
	}
	if m.Load1 != 0.42 || m.Load5 != 0.31 || m.Load15 != 0.22 {
		t.Errorf("load = %v %v %v", m.Load1, m.Load5, m.Load15)
	}
	if m.UptimeSeconds != 90061 {
		t.Errorf("UptimeSeconds = %v, want 90061", m.UptimeSeconds)
	}
	if m.Memory.TotalKB != 2013265 || m.Memory.AvailableKB != 1509949 {
		t.Errorf("memory = %+v", m.Memory)
	}
	if len(m.Disks) != 2 {
		t.Fatalf("got %d disks, want 2", len(m.Disks))
	}
	// Disks are sorted by mount so repeated samples align in a watch.
	if m.Disks[0].Mount != "/" || m.Disks[1].Mount != "/srv/apps" {
		t.Errorf("disks not sorted: %+v", m.Disks)
	}
	if m.Disks[0].TotalKB != 41152736 || m.Disks[0].UsedKB != 8230547 || m.Disks[0].AvailableKB != 30824861 {
		t.Errorf("disk values = %+v", m.Disks[0])
	}
}

func TestParseMetricsRejectsEmpty(t *testing.T) {
	if _, err := ParseMetrics(""); err == nil {
		t.Fatal("expected an error for empty output")
	}
}

func TestParseMetricsToleratesMissingFiles(t *testing.T) {
	// A host with no swap still reports the keys; a host where df matched
	// nothing simply has no disk lines and must not fail the whole snapshot.
	m, err := ParseMetrics("cpu_cores=1\nmem_total_kb=1000\nmem_available_kb=400\n")
	if err != nil {
		t.Fatalf("ParseMetrics: %v", err)
	}
	if len(m.Disks) != 0 {
		t.Errorf("disks = %+v, want none", m.Disks)
	}
	if m.Memory.UsedKB() != 600 {
		t.Errorf("UsedKB = %d, want 600", m.Memory.UsedKB())
	}
}

func TestPercentages(t *testing.T) {
	mem := MemoryMetrics{TotalKB: 1000, AvailableKB: 250}
	if got := mem.UsedPercent(); got != 75 {
		t.Errorf("memory UsedPercent = %v, want 75", got)
	}
	disk := DiskMetrics{TotalKB: 200, UsedKB: 50}
	if got := disk.UsedPercent(); got != 25 {
		t.Errorf("disk UsedPercent = %v, want 25", got)
	}
	// A zero total must not divide by zero.
	if got := (MemoryMetrics{}).UsedPercent(); got != 0 {
		t.Errorf("empty memory UsedPercent = %v, want 0", got)
	}
	// Available above total (possible on some kernels) must clamp, not wrap.
	if got := (MemoryMetrics{TotalKB: 100, AvailableKB: 200}).UsedKB(); got != 0 {
		t.Errorf("over-available UsedKB = %d, want 0", got)
	}
}

func TestClassifyThresholdsAndApps(t *testing.T) {
	h := &Health{
		Metrics: Metrics{
			CPUPercent: 95,
			Memory:     MemoryMetrics{TotalKB: 1000, AvailableKB: 50},
			Disks:      []DiskMetrics{{Mount: "/", TotalKB: 100, UsedKB: 96}},
		},
		Apps: []AppStatus{
			{Name: "web", Unit: "active", Healthy: true},
			{Name: "worker", Unit: "failed"},
			{Name: "new", NotDeployed: true},
		},
	}
	h.classify(HealthOptions{CPUMax: 90, MemMax: 90, DiskMax: 90})

	if !h.Degraded() {
		t.Fatal("expected the snapshot to be degraded")
	}
	for _, want := range []string{"cpu", "memory", "disk", "worker", "new"} {
		found := false
		for _, w := range h.Warnings {
			if strings.Contains(w, want) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("no warning mentioning %q in %v", want, h.Warnings)
		}
	}
}

func TestClassifyHealthy(t *testing.T) {
	h := &Health{
		Metrics: Metrics{
			CPUPercent: 5,
			Memory:     MemoryMetrics{TotalKB: 1000, AvailableKB: 800},
			Disks:      []DiskMetrics{{Mount: "/", TotalKB: 100, UsedKB: 20}},
		},
		Apps: []AppStatus{{Name: "web", Unit: "active", Healthy: true}},
	}
	h.classify(HealthOptions{CPUMax: 90, MemMax: 90, DiskMax: 90})
	if h.Degraded() {
		t.Fatalf("expected ok, got warnings %v", h.Warnings)
	}
}

func TestClassifyDisabledThresholds(t *testing.T) {
	h := &Health{Metrics: Metrics{CPUPercent: 100}}
	h.classify(HealthOptions{})
	if h.Degraded() {
		t.Fatalf("a zero threshold must disable the check, got %v", h.Warnings)
	}
}

func TestPrintHealth(t *testing.T) {
	var buf bytes.Buffer
	h := &Host{Name: "apps-prod", IP: "203.0.113.10", Out: &buf}
	health := &Health{
		Host:      "apps-prod",
		IP:        "203.0.113.10",
		SampledAt: time.Date(2026, 2, 11, 9, 30, 0, 0, time.UTC),
		Metrics: Metrics{
			Cores:         2,
			CPUPercent:    12.3,
			Load1:         0.10,
			Load5:         0.20,
			Load15:        0.30,
			UptimeSeconds: 90061,
			Memory:        MemoryMetrics{TotalKB: 2097152, AvailableKB: 1572864},
			Disks:         []DiskMetrics{{Mount: "/srv/apps", TotalKB: 41943040, UsedKB: 8388608, AvailableKB: 33554432}},
		},
		Apps: []AppStatus{{Name: "web", Domain: "web.example.com", Port: 3001, Unit: "active", Healthy: true}},
	}
	health.classify(HealthOptions{CPUMax: 90, MemMax: 90, DiskMax: 90})
	h.PrintHealth(health)

	out := buf.String()
	for _, want := range []string{
		"apps-prod (203.0.113.10)",
		"12.3%", "2 cores", "load 0.10 0.20 0.30",
		"memory", "25%",
		"/srv/apps", "20%",
		"uptime", "1d1h",
		"web", "ok",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "degraded") {
		t.Errorf("healthy snapshot reported degraded:\n%s", out)
	}
}

func TestPrintHealthDegraded(t *testing.T) {
	var buf bytes.Buffer
	h := &Host{Name: "apps-prod", IP: "203.0.113.10", Out: &buf}
	health := &Health{
		Host:    "apps-prod",
		IP:      "203.0.113.10",
		Metrics: Metrics{CPUPercent: 99},
		Apps:    []AppStatus{{Name: "web", Unit: "failed"}},
	}
	health.classify(HealthOptions{CPUMax: 90, MemMax: 90, DiskMax: 90})
	h.PrintHealth(health)

	out := buf.String()
	if !strings.Contains(out, "degraded") {
		t.Errorf("expected a degraded status:\n%s", out)
	}
	if !strings.Contains(out, "cpu") || !strings.Contains(out, "web") {
		t.Errorf("expected warnings for cpu and web:\n%s", out)
	}
}

func TestHumanKB(t *testing.T) {
	cases := map[uint64]string{
		512:             "512KB",
		2048:            "2MB",
		5 * 1024:        "5MB",
		3 * 1024 * 1024: "3.0GB",
	}
	for in, want := range cases {
		if got := humanKB(in); got != want {
			t.Errorf("humanKB(%d) = %q, want %q", in, got, want)
		}
	}
}

func TestHumanDuration(t *testing.T) {
	cases := map[time.Duration]string{
		time.Minute:      "1m",
		90 * time.Minute: "1h30m",
		26 * time.Hour:   "1d2h",
		-1:               "0m",
	}
	for in, want := range cases {
		if got := humanDuration(in); got != want {
			t.Errorf("humanDuration(%v) = %q, want %q", in, got, want)
		}
	}
}

// Guard against the metrics script drifting away from what ParseMetrics reads:
// every key it can emit must be one the parser knows.
func TestMetricsScriptKeysAreParsed(t *testing.T) {
	for _, key := range []string{"cpu_cores", "cpu_usage", "mem_total_kb", "mem_available_kb", "swap_total_kb", "swap_free_kb", "load1", "load5", "load15", "uptime_seconds", "disk "} {
		if !strings.Contains(metricsScript, key) {
			t.Errorf("metricsScript no longer emits %q", key)
		}
	}
}

func TestSharedAppsFiltering(t *testing.T) {
	h := &Host{Name: "apps-prod"}
	specs := []*appspec.Spec{
		{Name: "a"},
		{Name: "b", Host: "apps-prod"},
		{Name: "c", Host: "other"},
	}
	got := h.sharedApps(specs)
	if len(got) != 2 || got[0].Name != "a" || got[1].Name != "b" {
		t.Errorf("sharedApps = %+v", got)
	}
}
