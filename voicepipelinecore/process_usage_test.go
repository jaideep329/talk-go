package voicepipelinecore

import (
	"testing"
	"time"
)

func TestParseSchedstatRuntimeNS(t *testing.T) {
	got, err := parseSchedstatRuntimeNS([]byte("123456789 42 7\n"))
	if err != nil {
		t.Fatalf("parseSchedstatRuntimeNS error: %v", err)
	}
	if got != 123456789 {
		t.Fatalf("runtime ns = %d, want 123456789", got)
	}
}

func TestParseStatCPUJiffies(t *testing.T) {
	raw := []byte("123 (talk go) S 1 2 3 4 5 6 7 8 9 10 11 12 13 14 0 0 0 0\n")
	got, err := parseStatCPUJiffies(raw)
	if err != nil {
		t.Fatalf("parseStatCPUJiffies error: %v", err)
	}
	if got != 23 {
		t.Fatalf("cpu jiffies = %d, want 23", got)
	}
}

func TestParseStatusMemoryBytes(t *testing.T) {
	raw := []byte("Name:\ttalk-go\nVmHWM:\t  32100 kB\nVmRSS:\t  12345 kB\n")
	rss, hwm, err := parseStatusMemoryBytes(raw)
	if err != nil {
		t.Fatalf("parseStatusMemoryBytes error: %v", err)
	}
	if rss != 12345*1024 {
		t.Fatalf("rss = %d, want %d", rss, 12345*1024)
	}
	if hwm != 32100*1024 {
		t.Fatalf("hwm = %d, want %d", hwm, 32100*1024)
	}
}

func TestParseCgroupCPUStatV2(t *testing.T) {
	raw := []byte("usage_usec 2000000\nuser_usec 1500000\nsystem_usec 500000\nnr_periods 100\nnr_throttled 25\nthrottled_usec 300000\n")
	got, err := parseCgroupCPUStat(raw)
	if err != nil {
		t.Fatalf("parseCgroupCPUStat error: %v", err)
	}
	if got.usageUsec != 2_000_000 || got.nrPeriods != 100 || got.nrThrottled != 25 || got.throttledUsec != 300_000 {
		t.Fatalf("cgroup snapshot mismatch: %+v", got)
	}
}

func TestParseCgroupCPUStatV1(t *testing.T) {
	raw := []byte("nr_periods 100\nnr_throttled 25\nthrottled_time 300000000\n")
	got, err := parseCgroupCPUStat(raw)
	if err != nil {
		t.Fatalf("parseCgroupCPUStat error: %v", err)
	}
	if got.nrPeriods != 100 || got.nrThrottled != 25 || got.throttledUsec != 300_000 {
		t.Fatalf("cgroup snapshot mismatch: %+v", got)
	}
}

func TestCPUMcores(t *testing.T) {
	prev := processUsageSnapshot{pid: 1, alive: true, cpuTimeNS: 1_000_000_000}
	curr := processUsageSnapshot{pid: 1, alive: true, cpuTimeNS: 1_500_000_000}
	got := cpuMcores(prev, curr, 10*time.Second)
	if got != 50 {
		t.Fatalf("cpu mcores = %d, want 50", got)
	}
}

func TestContainerCPUMcores(t *testing.T) {
	prev := cgroupCPUSnapshot{available: true, usageUsec: 1_000_000}
	curr := cgroupCPUSnapshot{available: true, usageUsec: 1_750_000}
	got := containerCPUMcores(prev, curr, 10*time.Second)
	if got != 75 {
		t.Fatalf("container cpu mcores = %d, want 75", got)
	}
}

func TestThrottledFraction(t *testing.T) {
	got := throttledFraction(100, 25)
	if got != 0.25 {
		t.Fatalf("throttled fraction = %v, want 0.25", got)
	}
	if got := throttledFraction(0, 25); got != 0 {
		t.Fatalf("zero-period throttled fraction = %v, want 0", got)
	}
}
