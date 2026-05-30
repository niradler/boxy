package main

import (
	"bufio"
	"context"
	"os"
	"strconv"
	"strings"

	"go.opentelemetry.io/otel/metric"
)

const (
	cgroupMemCurrentPath = "/sys/fs/cgroup/memory.current"
	cgroupCPUStatPath    = "/sys/fs/cgroup/cpu.stat"
)

// Pod-level only: ephemeral nsjail execs have no stable per-sandbox cgroup; absent on non-cgroup-v2 hosts.
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
