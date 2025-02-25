package main

import (
	"github.com/prometheus/client_golang/prometheus"
	sglog "github.com/sourcegraph/log"
)

func mustRegisterMemoryMapMetrics(logger sglog.Logger) {
	logger = logger.Scoped("memoryMapMetrics")

	// The memory map metrics are collected via /proc, which
	// is only available on linux-based operating systems.

	// Instantiate shared FS objects for accessing /proc and /proc/self,
	// and skip metrics registration if we're aren't able to instantiate them
	// for whatever reason.

	// Register Prometheus memory map metrics

	prometheus.MustRegister(prometheus.NewGaugeFunc(prometheus.GaugeOpts{
		Name: "proc_metrics_memory_map_max_limit",
		Help: "Upper limit on amount of memory mapped regions a process may have.",
	}, func() float64 {
        return 0
	}))

	prometheus.MustRegister(prometheus.NewGaugeFunc(prometheus.GaugeOpts{
		Name: "proc_metrics_memory_map_current_count",
		Help: "Amount of memory mapped regions this process is currently using.",
	}, func() float64 {
        return 0
	}))
}
