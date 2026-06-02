package voicepipelinecore

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	processUsageLogInterval = 10 * time.Second
	fallbackProcClockTicks  = 100
)

type processUsageSnapshot struct {
	pid         int
	alive       bool
	cpuTimeNS   uint64
	rssBytes    uint64
	rssHWMBytes uint64
}

type cgroupCPUSnapshot struct {
	available     bool
	usageUsec     uint64
	nrPeriods     uint64
	nrThrottled   uint64
	throttledUsec uint64
}

type processUsageLog struct {
	Event                         string  `json:"event"`
	RoomName                      string  `json:"room_name,omitempty"`
	GoPID                         int     `json:"go_pid"`
	PythonPID                     int     `json:"python_pid"`
	SamplePeriodMS                int64   `json:"sample_period_ms"`
	GoAlive                       bool    `json:"go_alive"`
	PythonAlive                   bool    `json:"python_alive"`
	GoCPUMcores                   int64   `json:"go_cpu_mcores"`
	PythonCPUMcores               int64   `json:"python_cpu_mcores"`
	ContainerCPUAvailable         bool    `json:"container_cpu_available"`
	ContainerCPUMcores            int64   `json:"container_cpu_mcores"`
	ContainerCPUPeriods           uint64  `json:"container_cpu_periods"`
	ContainerCPUThrottledPeriods  uint64  `json:"container_cpu_throttled_periods"`
	ContainerCPUThrottledMS       uint64  `json:"container_cpu_throttled_ms"`
	ContainerCPUThrottledFraction float64 `json:"container_cpu_throttled_fraction"`
	GoRSSBytes                    uint64  `json:"go_rss_bytes"`
	PythonRSSBytes                uint64  `json:"python_rss_bytes"`
	GoRSSHWMBytes                 uint64  `json:"go_rss_hwm_bytes"`
	PythonRSSHWMBytes             uint64  `json:"python_rss_hwm_bytes"`
}

func (r *DailyRoom) monitorProcessUsage(goPID, pythonPID int) {
	prevGo, goErr := readProcessUsageSnapshot(goPID)
	prevPython, pythonErr := readProcessUsageSnapshot(pythonPID)
	prevCgroup, cgroupErr := readCgroupCPUSnapshot()
	if goErr != nil || pythonErr != nil || cgroupErr != nil {
		r.log("process_usage monitor init error: go_pid=%d go_err=%v python_pid=%d python_err=%v cgroup_err=%v", goPID, goErr, pythonPID, pythonErr, cgroupErr)
	}
	r.log("process_usage monitor started: go_pid=%d python_pid=%d interval=%s", goPID, pythonPID, processUsageLogInterval)

	lastAt := time.Now()
	ticker := time.NewTicker(processUsageLogInterval)
	defer ticker.Stop()

	done := taskDone(r.taskCtx)
	for {
		select {
		case <-ticker.C:
			now := time.Now()
			currGo, currGoErr := readProcessUsageSnapshot(goPID)
			currPython, currPythonErr := readProcessUsageSnapshot(pythonPID)
			currCgroup, currCgroupErr := readCgroupCPUSnapshot()
			if currGoErr != nil || currPythonErr != nil || currCgroupErr != nil {
				r.log("process_usage read error: go_pid=%d go_err=%v python_pid=%d python_err=%v cgroup_err=%v", goPID, currGoErr, pythonPID, currPythonErr, currCgroupErr)
			}
			r.emitProcessUsage(prevGo, currGo, prevPython, currPython, prevCgroup, currCgroup, now.Sub(lastAt))
			prevGo = currGo
			prevPython = currPython
			prevCgroup = currCgroup
			lastAt = now
		case <-r.waitDone:
			return
		case <-done:
			return
		}
	}
}

func (r *DailyRoom) emitProcessUsage(prevGo, currGo, prevPython, currPython processUsageSnapshot, prevCgroup, currCgroup cgroupCPUSnapshot, elapsed time.Duration) {
	periods := uint64Delta(prevCgroup.nrPeriods, currCgroup.nrPeriods)
	throttledPeriods := uint64Delta(prevCgroup.nrThrottled, currCgroup.nrThrottled)
	entry := processUsageLog{
		Event:                         "process_usage",
		RoomName:                      r.roomName,
		GoPID:                         currGo.pid,
		PythonPID:                     currPython.pid,
		SamplePeriodMS:                elapsed.Milliseconds(),
		GoAlive:                       currGo.alive,
		PythonAlive:                   currPython.alive,
		GoCPUMcores:                   cpuMcores(prevGo, currGo, elapsed),
		PythonCPUMcores:               cpuMcores(prevPython, currPython, elapsed),
		ContainerCPUAvailable:         currCgroup.available,
		ContainerCPUMcores:            containerCPUMcores(prevCgroup, currCgroup, elapsed),
		ContainerCPUPeriods:           periods,
		ContainerCPUThrottledPeriods:  throttledPeriods,
		ContainerCPUThrottledMS:       uint64Delta(prevCgroup.throttledUsec, currCgroup.throttledUsec) / 1000,
		ContainerCPUThrottledFraction: throttledFraction(periods, throttledPeriods),
		GoRSSBytes:                    currGo.rssBytes,
		PythonRSSBytes:                currPython.rssBytes,
		GoRSSHWMBytes:                 currGo.rssHWMBytes,
		PythonRSSHWMBytes:             currPython.rssHWMBytes,
	}
	raw, err := json.Marshal(entry)
	if err != nil {
		r.log("process_usage marshal error: %v", err)
		return
	}
	r.log("process_usage %s", raw)
}

func cpuMcores(prev, curr processUsageSnapshot, elapsed time.Duration) int64 {
	if elapsed <= 0 || !prev.alive || !curr.alive || curr.cpuTimeNS < prev.cpuTimeNS {
		return 0
	}
	deltaCPU := curr.cpuTimeNS - prev.cpuTimeNS
	return int64(math.Round(float64(deltaCPU) / float64(elapsed.Nanoseconds()) * 1000))
}

func containerCPUMcores(prev, curr cgroupCPUSnapshot, elapsed time.Duration) int64 {
	if elapsed <= 0 || !prev.available || !curr.available || curr.usageUsec < prev.usageUsec {
		return 0
	}
	deltaUsec := curr.usageUsec - prev.usageUsec
	return int64(math.Round(float64(deltaUsec) / float64(elapsed.Microseconds()) * 1000))
}

func throttledFraction(periods, throttledPeriods uint64) float64 {
	if periods == 0 {
		return 0
	}
	return float64(throttledPeriods) / float64(periods)
}

func uint64Delta(prev, curr uint64) uint64 {
	if curr < prev {
		return 0
	}
	return curr - prev
}

func readProcessUsageSnapshot(pid int) (processUsageSnapshot, error) {
	snap := processUsageSnapshot{pid: pid, alive: true}
	cpuNS, err := readProcessCPUTimeNS(pid)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			snap.alive = false
			return snap, nil
		}
		return snap, err
	}
	rss, hwm, err := readProcessMemoryBytes(pid)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			snap.alive = false
			return snap, nil
		}
		return snap, err
	}
	snap.cpuTimeNS = cpuNS
	snap.rssBytes = rss
	snap.rssHWMBytes = hwm
	return snap, nil
}

func readCgroupCPUSnapshot() (cgroupCPUSnapshot, error) {
	type candidate struct {
		statPath  string
		usagePath string
	}
	candidates := []candidate{
		{statPath: "/sys/fs/cgroup/cpu.stat"},
		{statPath: "/sys/fs/cgroup/cpu,cpuacct/cpu.stat", usagePath: "/sys/fs/cgroup/cpu,cpuacct/cpuacct.usage"},
		{statPath: "/sys/fs/cgroup/cpu/cpu.stat", usagePath: "/sys/fs/cgroup/cpuacct/cpuacct.usage"},
	}
	var firstErr error
	for _, cand := range candidates {
		raw, err := os.ReadFile(cand.statPath)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		snap, err := parseCgroupCPUStat(raw)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		if snap.usageUsec == 0 && cand.usagePath != "" {
			usageRaw, err := os.ReadFile(cand.usagePath)
			if err == nil {
				if usageNS, parseErr := strconv.ParseUint(strings.TrimSpace(string(usageRaw)), 10, 64); parseErr == nil {
					snap.usageUsec = usageNS / 1000
				}
			}
		}
		snap.available = true
		return snap, nil
	}
	if firstErr != nil {
		return cgroupCPUSnapshot{}, firstErr
	}
	return cgroupCPUSnapshot{}, os.ErrNotExist
}

func readProcessCPUTimeNS(pid int) (uint64, error) {
	ns, err := readProcessSchedstatNS(pid)
	if err == nil {
		return ns, nil
	}
	jiffies, fallbackErr := readProcessStatJiffies(pid)
	if fallbackErr != nil {
		return 0, err
	}
	return jiffies * uint64(time.Second) / fallbackProcClockTicks, nil
}

func readProcessSchedstatNS(pid int) (uint64, error) {
	taskDir := fmt.Sprintf("/proc/%d/task", pid)
	entries, err := os.ReadDir(taskDir)
	if err != nil {
		return 0, err
	}
	var total uint64
	var readAny bool
	var firstErr error
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(taskDir, entry.Name(), "schedstat"))
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		runtimeNS, err := parseSchedstatRuntimeNS(raw)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		total += runtimeNS
		readAny = true
	}
	if readAny {
		return total, nil
	}
	if firstErr != nil {
		return 0, firstErr
	}
	return 0, os.ErrNotExist
}

func readProcessStatJiffies(pid int) (uint64, error) {
	taskDir := fmt.Sprintf("/proc/%d/task", pid)
	entries, err := os.ReadDir(taskDir)
	if err != nil {
		return 0, err
	}
	var total uint64
	var readAny bool
	var firstErr error
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(taskDir, entry.Name(), "stat"))
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		jiffies, err := parseStatCPUJiffies(raw)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		total += jiffies
		readAny = true
	}
	if readAny {
		return total, nil
	}
	if firstErr != nil {
		return 0, firstErr
	}
	return 0, os.ErrNotExist
}

func readProcessMemoryBytes(pid int) (rssBytes, hwmBytes uint64, err error) {
	raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
	if err != nil {
		return 0, 0, err
	}
	return parseStatusMemoryBytes(raw)
}

func parseCgroupCPUStat(raw []byte) (cgroupCPUSnapshot, error) {
	var snap cgroupCPUSnapshot
	for _, line := range strings.Split(string(raw), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		value, err := strconv.ParseUint(fields[1], 10, 64)
		if err != nil {
			return cgroupCPUSnapshot{}, err
		}
		switch fields[0] {
		case "usage_usec":
			snap.usageUsec = value
		case "nr_periods":
			snap.nrPeriods = value
		case "nr_throttled":
			snap.nrThrottled = value
		case "throttled_usec":
			snap.throttledUsec = value
		case "throttled_time":
			snap.throttledUsec = value / 1000
		}
	}
	return snap, nil
}

func parseSchedstatRuntimeNS(raw []byte) (uint64, error) {
	fields := strings.Fields(string(raw))
	if len(fields) == 0 {
		return 0, errors.New("empty schedstat")
	}
	return strconv.ParseUint(fields[0], 10, 64)
}

func parseStatCPUJiffies(raw []byte) (uint64, error) {
	s := string(raw)
	endComm := strings.LastIndex(s, ")")
	if endComm < 0 || endComm+2 >= len(s) {
		return 0, errors.New("malformed stat")
	}
	fields := strings.Fields(s[endComm+2:])
	if len(fields) <= 12 {
		return 0, errors.New("stat missing cpu fields")
	}
	utime, err := strconv.ParseUint(fields[11], 10, 64)
	if err != nil {
		return 0, err
	}
	stime, err := strconv.ParseUint(fields[12], 10, 64)
	if err != nil {
		return 0, err
	}
	return utime + stime, nil
}

func parseStatusMemoryBytes(raw []byte) (rssBytes, hwmBytes uint64, err error) {
	for _, line := range strings.Split(string(raw), "\n") {
		switch {
		case strings.HasPrefix(line, "VmRSS:"):
			rssBytes, err = parseStatusKBLine(line)
			if err != nil {
				return 0, 0, err
			}
		case strings.HasPrefix(line, "VmHWM:"):
			hwmBytes, err = parseStatusKBLine(line)
			if err != nil {
				return 0, 0, err
			}
		}
	}
	return rssBytes, hwmBytes, nil
}

func parseStatusKBLine(line string) (uint64, error) {
	fields := strings.Fields(line)
	if len(fields) < 2 {
		return 0, fmt.Errorf("malformed status memory line %q", line)
	}
	kb, err := strconv.ParseUint(fields[1], 10, 64)
	if err != nil {
		return 0, err
	}
	return kb * 1024, nil
}
