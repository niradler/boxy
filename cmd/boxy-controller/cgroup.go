package main

import (
	"bufio"
	"context"
	"os"
	"strconv"
	"strings"

	"go.opentelemetry.io/otel/metric"
)

// cgroup v2 unified-hierarchy files, as seen from inside the container's own
// cgroup namespace (the controller pod's cgroup root).
const (
	cgroupMemCurrentPath = "/sys/fs/cgroup/memory.current"
	cgroupCPUStatPath    = "/sys/fs/cgroup/cpu.stat"
)

// registerPodResourceGauges wires observable instruments that read the
// controller pod's own cgroup at scrape time. These are pod-level (no
// sandbox_id): per-jail cpu/mem is not available because each exec is an
// ephemeral nsjail process with no stable per-sandbox cgroup (see
// .claude/docs/agent-sandbox-adoption.md, FOCUS #3). On platforms without
// cgroup v2 (dev/Windows) the files are absent and the callbacks observe
// nothing.
func registerPodResourceGauges(m metric.Meter) error {
	memGauge, err := m.Int64ObservableGauge(
		"boxy.controller.pod.memory.usage",
		metric.WithDescription("Controller pod cgroup memory.current"),
		metric.WithUnit("By"),
	)
	if err != nil {
		return err
	}
	cpuCounter, err := m.Float64ObservableCounter(
		"boxy.controller.pod.cpu.time",
		metric.WithDescription("Controller pod cgroup cumulative CPU time"),
		metric.WithUnit("s"),
	)
	if err != nil {
		return err
	}

	_, err = m.RegisterCallback(func(_ context.Context, o metric.Observer) error {
		if v, ok := readCgroupMemoryBytes(); ok {
			o.ObserveInt64(memGauge, int64(v))
		}
		if v, ok := readCgroupCPUSeconds(); ok {
			o.ObserveFloat64(cpuCounter, v)
		}
		return nil
	}, memGauge, cpuCounter)
	return err
}

// readCgroupMemoryBytes reads memory.current. Returns false if unavailable.
func readCgroupMemoryBytes() (uint64, bool) {
	data, err := os.ReadFile(cgroupMemCurrentPath)
	if err != nil {
		return 0, false
	}
	n, err := strconv.ParseUint(strings.TrimSpace(string(data)), 10, 64)
	if err != nil {
		return 0, false
	}
	return n, true
}

// readCgroupCPUSeconds reads usage_usec from cpu.stat and converts to seconds.
func readCgroupCPUSeconds() (float64, bool) {
	f, err := os.Open(cgroupCPUStatPath)
	if err != nil {
		return 0, false
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) == 2 && fields[0] == "usage_usec" {
			usec, err := strconv.ParseUint(fields[1], 10, 64)
			if err != nil {
				return 0, false
			}
			return float64(usec) / 1e6, true
		}
	}
	return 0, false
}
