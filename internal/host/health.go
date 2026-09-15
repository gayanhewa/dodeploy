package host

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gayanhewa/dodeploy/internal/appspec"
)

// ErrDegraded reports that a host answered but something about it is wrong: a
// threshold was crossed or an app is unhealthy. Callers use it to drive an exit
// code, so a monitor can alert without scraping the report.
var ErrDegraded = errors.New("host is degraded")

// HealthOptions holds the thresholds that turn a measurement into a warning.
// A zero threshold disables that check.
type HealthOptions struct {
	// CPUMax flags CPU use above this percentage.
	CPUMax float64
	// MemMax flags memory use above this percentage.
	MemMax float64
	// DiskMax flags any filesystem used above this percentage.
	DiskMax float64
}

// MemoryMetrics is memory use in kilobytes.
type MemoryMetrics struct {
	TotalKB     uint64 `json:"total_kb"`
	AvailableKB uint64 `json:"available_kb"`
	SwapTotalKB uint64 `json:"swap_total_kb"`
	SwapFreeKB  uint64 `json:"swap_free_kb"`
}

// UsedKB is memory that is not available to new allocations.
func (m MemoryMetrics) UsedKB() uint64 {
	if m.AvailableKB >= m.TotalKB {
		return 0
	}
	return m.TotalKB - m.AvailableKB
}

// UsedPercent is used memory as a percentage of total.
func (m MemoryMetrics) UsedPercent() float64 {
	if m.TotalKB == 0 {
		return 0
	}
	return float64(m.UsedKB()) / float64(m.TotalKB) * 100
}

// DiskMetrics is one filesystem's use in kilobytes.
type DiskMetrics struct {
	Mount       string `json:"mount"`
	TotalKB     uint64 `json:"total_kb"`
	UsedKB      uint64 `json:"used_kb"`
	AvailableKB uint64 `json:"available_kb"`
}

// UsedPercent is used space as a percentage of total.
func (d DiskMetrics) UsedPercent() float64 {
	if d.TotalKB == 0 {
		return 0
	}
	return float64(d.UsedKB) / float64(d.TotalKB) * 100
}

// Metrics is a point-in-time snapshot of a host's resource use.
type Metrics struct {
	Cores         int           `json:"cores"`
	CPUPercent    float64       `json:"cpu_percent"`
	Load1         float64       `json:"load1"`
	Load5         float64       `json:"load5"`
	Load15        float64       `json:"load15"`
	UptimeSeconds float64       `json:"uptime_seconds"`
	Memory        MemoryMetrics `json:"memory"`
	Disks         []DiskMetrics `json:"disks"`
}

// Health is a metrics snapshot plus the state of the apps on the host.
type Health struct {
	Host      string      `json:"host"`
	IP        string      `json:"ip"`
	SampledAt time.Time   `json:"sampled_at"`
	Metrics   Metrics     `json:"metrics"`
	Apps      []AppStatus `json:"apps,omitempty"`
	// Warnings are the human-readable reasons the host is degraded.
	Warnings []string `json:"warnings,omitempty"`
}

// Degraded reports whether anything crossed a threshold or failed.
func (h *Health) Degraded() bool { return len(h.Warnings) > 0 }

// Health samples the host and the apps expected on it.
//
// It is deliberately one round trip for the system metrics and one probe per app,
// so a health check on a small droplet stays cheap enough to run from a monitor
// on a short interval. The app probes run after the metrics, so a host that is
// unreachable fails on the first call with a clear error.
func (h *Host) Health(ctx context.Context, specs []*appspec.Spec, opts HealthOptions) (*Health, error) {
	if err := h.ensureResolved(ctx); err != nil {
		return nil, err
	}

	out, err := h.Remote.Run(ctx, metricsScript)
	if err != nil {
		return nil, err
	}
	metrics, err := ParseMetrics(out)
	if err != nil {
		return nil, err
	}

	apps := h.sharedApps(specs)
	health := &Health{
		Host:      h.Name,
		IP:        h.IP,
		SampledAt: time.Now(),
		Metrics:   metrics,
		Apps:      h.appStatuses(ctx, apps, true),
	}
	health.classify(opts)
	return health, nil
}

// classify records every reason the snapshot should be considered degraded.
func (h *Health) classify(opts HealthOptions) {
	m := h.Metrics
	if opts.CPUMax > 0 && m.CPUPercent > opts.CPUMax {
		h.Warnings = append(h.Warnings, fmt.Sprintf("cpu at %.1f%%, over the %.0f%% limit", m.CPUPercent, opts.CPUMax))
	}
	if opts.MemMax > 0 && m.Memory.UsedPercent() > opts.MemMax {
		h.Warnings = append(h.Warnings, fmt.Sprintf("memory at %.0f%%, over the %.0f%% limit",
			m.Memory.UsedPercent(), opts.MemMax))
	}
	for _, d := range m.Disks {
		if opts.DiskMax > 0 && d.UsedPercent() > opts.DiskMax {
			h.Warnings = append(h.Warnings, fmt.Sprintf("disk %s at %.0f%%, over the %.0f%% limit",
				d.Mount, d.UsedPercent(), opts.DiskMax))
		}
	}
	for _, a := range h.Apps {
		switch {
		case a.NotDeployed:
			h.Warnings = append(h.Warnings, fmt.Sprintf("app %s is not deployed", a.Name))
		case !a.Healthy:
			h.Warnings = append(h.Warnings, fmt.Sprintf("app %s is unhealthy (unit %s)", a.Name, a.Unit))
		case a.Unit != "active":
			h.Warnings = append(h.Warnings, fmt.Sprintf("app %s is %s", a.Name, a.Unit))
		}
	}
}

// PrintHealth renders a snapshot for a person. It is stable enough to read in a
// terminal and to grep in a log.
func (h *Host) PrintHealth(health *Health) {
	m := health.Metrics
	fmt.Fprintf(h.Out, "host %s (%s) at %s\n", health.Host, health.IP, health.SampledAt.Format(time.RFC3339))
	fmt.Fprintf(h.Out, "  cpu      %5.1f%%  %d cores  load %s\n",
		m.CPUPercent, m.Cores, loadString(m))
	mem := m.Memory
	fmt.Fprintf(h.Out, "  memory   %s / %s (%3.0f%%)  swap %s / %s\n",
		humanKB(mem.UsedKB()), humanKB(mem.TotalKB), mem.UsedPercent(),
		humanKB(mem.SwapTotalKB-mem.SwapFreeKB), humanKB(mem.SwapTotalKB))
	for _, d := range m.Disks {
		fmt.Fprintf(h.Out, "  disk     %-12s %s / %s (%3.0f%%)\n",
			d.Mount, humanKB(d.UsedKB), humanKB(d.TotalKB), d.UsedPercent())
	}
	fmt.Fprintf(h.Out, "  uptime   %s\n", humanDuration(time.Duration(m.UptimeSeconds)*time.Second))

	if len(health.Apps) > 0 {
		fmt.Fprintln(h.Out)
		fmt.Fprintf(h.Out, "  %-22s %-34s %-8s %-10s %s\n", "APP", "DOMAIN", "PORT", "UNIT", "HEALTH")
		for _, st := range health.Apps {
			printStatus(h, st)
		}
	}

	fmt.Fprintln(h.Out)
	if health.Degraded() {
		fmt.Fprintf(h.Out, "  status   degraded\n")
		for _, w := range health.Warnings {
			fmt.Fprintf(h.Out, "    - %s\n", w)
		}
		return
	}
	fmt.Fprintf(h.Out, "  status   ok\n")
}

// metricsScript gathers the system numbers in one ssh round trip.
//
// Everything comes from /proc and df, which are present on any Ubuntu image, so
// a bare droplet needs no agent or package installed to be monitored. CPU use is
// a delta between two /proc/stat samples, because a single sample only gives
// totals since boot.
const metricsScript = `set -u
cores=$(nproc 2>/dev/null || getconf _NPROCESSORS_ONLN 2>/dev/null || echo 1)
echo "cpu_cores=$cores"

awk 'BEGIN {
  while ((getline line < "/proc/stat") > 0) {
    if (line ~ /^cpu /) { split(line, a, /[ \t]+/); break }
  }
  close("/proc/stat")
  t1 = a[2]+a[3]+a[4]+a[5]+a[6]+a[7]+a[8]+a[9]
  i1 = a[5]+a[6]
  system("sleep 0.5")
  while ((getline line < "/proc/stat") > 0) {
    if (line ~ /^cpu /) { split(line, b, /[ \t]+/); break }
  }
  close("/proc/stat")
  t2 = b[2]+b[3]+b[4]+b[5]+b[6]+b[7]+b[8]+b[9]
  i2 = b[5]+b[6]
  dt = t2-t1; di = i2-i1
  if (dt > 0) printf "cpu_usage=%.1f\n", 100*(dt-di)/dt
  else print "cpu_usage=0"
}'

awk '/^MemTotal:/{t=$2} /^MemAvailable:/{a=$2} /^SwapTotal:/{s=$2} /^SwapFree:/{f=$2}
     END {printf "mem_total_kb=%d\nmem_available_kb=%d\nswap_total_kb=%d\nswap_free_kb=%d\n", t, a, s, f}' /proc/meminfo

awk '{printf "load1=%s\nload5=%s\nload15=%s\n", $1, $2, $3}' /proc/loadavg
awk '{printf "uptime_seconds=%.0f\n", $1}' /proc/uptime

df -P -k / /srv/apps 2>/dev/null | awk 'NR > 1 && $6 != "" {print "disk " $6 " " $2 " " $3 " " $4}'
`

// ParseMetrics reads the key/value output of metricsScript.
//
// Unknown lines are ignored so a newer script than the binary cannot break an
// older client.
func ParseMetrics(out string) (Metrics, error) {
	var m Metrics
	saw := map[string]bool{}

	for _, raw := range strings.Split(out, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" {
			continue
		}

		// Most lines are key=value; the disk lines are space separated because
		// there are several values per filesystem.
		if key, value, ok := strings.Cut(line, "="); ok {
			value = strings.TrimSpace(value)
			switch key {
			case "cpu_cores":
				m.Cores = int(atuint(value))
			case "cpu_usage":
				m.CPUPercent = atfloat(value)
			case "load1":
				m.Load1 = atfloat(value)
			case "load5":
				m.Load5 = atfloat(value)
			case "load15":
				m.Load15 = atfloat(value)
			case "uptime_seconds":
				m.UptimeSeconds = atfloat(value)
			case "mem_total_kb":
				m.Memory.TotalKB = atuint(value)
			case "mem_available_kb":
				m.Memory.AvailableKB = atuint(value)
			case "swap_total_kb":
				m.Memory.SwapTotalKB = atuint(value)
			case "swap_free_kb":
				m.Memory.SwapFreeKB = atuint(value)
			default:
				continue
			}
			saw[key] = true
			continue
		}

		fields := strings.Fields(line)
		if fields[0] == "disk" && len(fields) >= 5 {
			m.Disks = append(m.Disks, DiskMetrics{
				Mount:       fields[1],
				TotalKB:     atuint(fields[2]),
				UsedKB:      atuint(fields[3]),
				AvailableKB: atuint(fields[4]),
			})
		}
	}

	if !saw["mem_total_kb"] && !saw["cpu_cores"] {
		return m, fmt.Errorf("no metrics in host output; is this an Ubuntu host?")
	}
	sort.Slice(m.Disks, func(i, j int) bool { return m.Disks[i].Mount < m.Disks[j].Mount })
	return m, nil
}

// The values below come from the script's own output, which is trusted: a parse
// failure becomes a zero rather than an error, so one missing file cannot take
// down a whole health check.
func atuint(s string) uint64 {
	v, _ := strconv.ParseUint(s, 10, 64)
	return v
}

func atfloat(s string) float64 {
	v, _ := strconv.ParseFloat(s, 64)
	return v
}

func loadString(m Metrics) string {
	return fmt.Sprintf("%.2f %.2f %.2f", m.Load1, m.Load5, m.Load15)
}

func humanKB(kb uint64) string {
	const (
		mb = 1 << 10
		gb = 1 << 20
		tb = 1 << 30
	)
	switch {
	case kb >= tb:
		return fmt.Sprintf("%.1fTB", float64(kb)/tb)
	case kb >= gb:
		return fmt.Sprintf("%.1fGB", float64(kb)/gb)
	case kb >= mb:
		return fmt.Sprintf("%.0fMB", float64(kb)/mb)
	default:
		return fmt.Sprintf("%dKB", kb)
	}
}

func humanDuration(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	days := int(d.Hours()) / 24
	hours := int(d.Hours()) % 24
	minutes := int(d.Minutes()) % 60
	switch {
	case days > 0:
		return fmt.Sprintf("%dd%dh", days, hours)
	case hours > 0:
		return fmt.Sprintf("%dh%dm", hours, minutes)
	default:
		return fmt.Sprintf("%dm", minutes)
	}
}
