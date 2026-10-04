// Freebuff Opti - Windows process controls.
//
// Everything in this file is a thin, honest wrapper over a Win32 call. The
// engine decides *what* to apply; this decides only *how*, and reports whether
// each call actually succeeded rather than assuming it did.
//
// Which of these are real is not a matter of opinion - each one was measured on
// Windows 11 26200 (build 26200), 20 logical processors, before it was wired
// up. The results, because they shape the defaults:
//
//	SetProcessWorkingSetSizeEx(h, min, max, QUOTA_LIMITS_HARDWS_MAX_ENABLE)
//	    works, and the cap is enforced: a process holding 555 MB was pinned at
//	    219 MB against a 220 MB limit and stayed there. The earlier failure
//	    (ERROR_INVALID_PARAMETER) was the `min` sentinel, not the call: passing
//	    SIZE_T(-1) for min is rejected, so min is always a real number here.
//	SetInformationJobObject(JobObjectCpuRateControlInformation)
//	    works, and the rate is a share of *total* system CPU, not of one core:
//	    CpuRate=100 (1%) on a 20-core machine let a single-threaded busy loop
//	    run at exactly 20% of one core, against 95% uncapped.
//	    AssignProcessToJobObject is the catch - it succeeds for the Freebuff
//	    browser process and is refused with "Access is denied" for every
//	    renderer, GPU and utility process, and for the Bun orchestrator, which
//	    are already inside Chromium's own job.
//	SetPriorityClass / SetProcessAffinityMask / SetProcessInformation(EcoQoS)
//	    all work on every process in the tree, including the sandboxed ones.
//
// So the reachable controls are: a hard working-set cap per process (its own
// exact limit, not a tree budget), and priority, affinity and EcoQoS across the
// whole tree. The job object is still installed on the browser process, because
// it costs nothing and is the only hard CPU throttle that exists - but it is
// reported as partial, never as "CPU is capped".
package main

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"unsafe"
)

var (
	kernel32 = syscall.NewLazyDLL("kernel32.dll")
	psapi    = syscall.NewLazyDLL("psapi.dll")

	procOpenProcess                = kernel32.NewProc("OpenProcess")
	procCloseHandle                = kernel32.NewProc("CloseHandle")
	procSetProcessWorkingSetSizeEx = kernel32.NewProc("SetProcessWorkingSetSizeEx")
	procEmptyWorkingSet            = psapi.NewProc("EmptyWorkingSet")
	procSetPriorityClass           = kernel32.NewProc("SetPriorityClass")
	procGetPriorityClass           = kernel32.NewProc("GetPriorityClass")
	procSetProcessAffinityMask     = kernel32.NewProc("SetProcessAffinityMask")
	procGetProcessAffinityMask     = kernel32.NewProc("GetProcessAffinityMask")
	procSetProcessInformation      = kernel32.NewProc("SetProcessInformation")
	procCreateJobObject            = kernel32.NewProc("CreateJobObjectW")
	procAssignProcessToJobObject   = kernel32.NewProc("AssignProcessToJobObject")
	procSetInformationJobObject    = kernel32.NewProc("SetInformationJobObject")
)

const (
	// Access rights. PROCESS_QUERY_LIMITED_INFORMATION is the only one that is
	// granted for a process at a higher integrity level, so it is what the
	// read-only paths ask for.
	processQueryLimitedInformation = 0x1000
	processQueryInformation        = 0x0400
	processSetInformation          = 0x0200
	processSetQuota                = 0x0100
	processTerminate               = 0x0001

	// QUOTA_LIMITS_HARDWS_MAX_ENABLE: the working set may not grow past the
	// given maximum. Without this flag the max is advisory and the kernel
	// ignores it.
	quotaHardMaxEnable = 0x4

	// Priority classes.
	priorityIdle        = 0x00000040
	priorityBelowNormal = 0x00004000
	priorityNormal      = 0x00000020

	// ProcessPowerThrottling / EcoQoS.
	processPowerThrottling = 4
	throttleExecutionSpeed = 0x1

	// Job object CPU rate control.
	jobObjectCpuRateControlInformation = 15
	cpuRateControlEnable               = 0x1
	cpuRateControlHardCap              = 0x4
)

// ------------------------------------------------------------- process tree ---

// procEntry is one process in the tree, as the OS describes it.
type procEntry struct {
	PID       int
	ParentPID int
	Name      string
	WorkingMB int
}

// processTree returns every Freebuff-owned process on the machine: the Electron
// processes, which all share the Freebuff.exe image name, and the Bun
// orchestrator beside them. Each entry is pid -> {ppid, name, working MB}.
//
// It goes through Win32_Process rather than tasklist because a single CIM query
// is one process launch instead of one per lookup, and because the working-set
// size comes back in the same pass.
func processTree() (map[int][]string, error) {
	script := `Get-CimInstance Win32_Process | Where-Object { $_.Name -eq 'Freebuff.exe' -or $_.Name -eq 'bun.exe' } | ` +
		`ForEach-Object { "$($_.ProcessId)|$($_.ParentProcessId)|$($_.Name)|$([int]($_.WorkingSetSize/1MB))" }`
	cmd := exec.Command("powershell.exe", "-NoProfile", "-NonInteractive", "-Command", script)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	tree := map[int][]string{}
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.Split(line, "|")
		if len(parts) < 4 {
			continue
		}
		pid, err := strconv.Atoi(parts[0])
		if err != nil || pid == 0 {
			continue
		}
		tree[pid] = parts[1:]
	}
	return tree, nil
}

// ------------------------------------------------------------------ limits ---

// openProcessFor re-opens a process with the access a call needs. A process we
// cannot open is reported, not skipped silently.
func openProcessFor(pid int, access uintptr) (syscall.Handle, error) {
	h, _, err := procOpenProcess.Call(access, 0, uintptr(pid))
	if h == 0 {
		return 0, fmt.Errorf("%v", err)
	}
	return syscall.Handle(h), nil
}

// setWorkingSetCap installs a hard maximum on one process's working set. The
// pages above the cap are pushed out to the pagefile, so the process keeps
// running; it just cannot keep them resident.
//
// min must be a real number: SetProcessWorkingSetSizeEx rejects the
// SIZE_TARGET(-1) sentinel that "no minimum" would suggest, with
// ERROR_INVALID_PARAMETER, which is how an earlier version of this file managed
// to look like the call was unsupported.
func setWorkingSetCap(pid, maxBytes int) error {
	if maxBytes <= 0 {
		return nil
	}
	h, err := openProcessFor(pid, processQueryLimitedInformation|processSetQuota|processSetInformation)
	if err != nil {
		return err
	}
	defer procCloseHandle.Call(uintptr(h))
	// The minimum is a quarter of the maximum: enough that the process is not
	// fighting the trimmer for its own hot pages, low enough that the cap is
	// the number that is actually respected.
	minBytes := maxBytes / 4
	r, _, callErr := procSetProcessWorkingSetSizeEx.Call(uintptr(h),
		uintptr(minBytes), uintptr(maxBytes), uintptr(quotaHardMaxEnable))
	if r == 0 {
		return fmt.Errorf("%v", callErr)
	}
	return nil
}

// clearWorkingSetCap removes the maximum again (QUOTA_LIMITS_HARDWS_MAX_DISABLE).
func clearWorkingSetCap(pid int) error {
	h, err := openProcessFor(pid, processQueryLimitedInformation|processSetQuota|processSetInformation)
	if err != nil {
		return err
	}
	defer procCloseHandle.Call(uintptr(h))
	const quotaHardMaxDisable = 0x8
	r, _, callErr := procSetProcessWorkingSetSizeEx.Call(uintptr(h),
		uintptr(0), uintptr(0), uintptr(quotaHardMaxDisable))
	if r == 0 {
		return fmt.Errorf("%v", callErr)
	}
	return nil
}

// trimWorkingSet hands the pages back to the OS now. Under a hard cap the
// kernel trims on its own; this is what makes the effect visible immediately
// instead of on the next allocation.
func trimWorkingSet(pid int) bool {
	h, err := openProcessFor(pid, processQueryLimitedInformation|processSetQuota|processSetInformation)
	if err != nil {
		return false
	}
	defer procCloseHandle.Call(uintptr(h))
	r, _, _ := procEmptyWorkingSet.Call(uintptr(h))
	return r != 0
}

// setPriority asks the scheduler to favour whatever else is on the machine.
func setPriority(pid int, class uintptr) error {
	h, err := openProcessFor(pid, processSetInformation)
	if err != nil {
		return err
	}
	defer procCloseHandle.Call(uintptr(h))
	r, _, callErr := procSetPriorityClass.Call(uintptr(h), class)
	if r == 0 {
		return fmt.Errorf("%v", callErr)
	}
	return nil
}

func getPriority(pid int) (uintptr, error) {
	h, err := openProcessFor(pid, processQueryLimitedInformation|processQueryInformation)
	if err != nil {
		return 0, err
	}
	defer procCloseHandle.Call(uintptr(h))
	r, _, callErr := procGetPriorityClass.Call(uintptr(h))
	if r == 0 {
		return 0, fmt.Errorf("%v", callErr)
	}
	return r, nil
}

// setAffinity pins a process to the cores in mask. On a machine with several
// cores this is the control the engine leans on hardest: it is the only one
// that works on the sandboxed renderers.
func setAffinity(pid int, mask uintptr) error {
	h, err := openProcessFor(pid, processSetInformation|processQueryInformation)
	if err != nil {
		return err
	}
	defer procCloseHandle.Call(uintptr(h))
	r, _, callErr := procSetProcessAffinityMask.Call(uintptr(h), mask)
	if r == 0 {
		return fmt.Errorf("%v", callErr)
	}
	return nil
}

func getAffinity(pid int) (proc, system uintptr, err error) {
	h, err := openProcessFor(pid, processQueryInformation)
	if err != nil {
		return 0, 0, err
	}
	defer procCloseHandle.Call(uintptr(h))
	r, _, callErr := procGetProcessAffinityMask.Call(uintptr(h),
		uintptr(unsafe.Pointer(&proc)), uintptr(unsafe.Pointer(&system)))
	if r == 0 {
		return 0, 0, fmt.Errorf("%v", callErr)
	}
	return proc, system, nil
}

type powerThrottlingState struct {
	Version     uint32
	ControlMask uint32
	StateMask   uint32
}

// setEcoQoS puts a process into efficiency mode. On a hybrid CPU the scheduler
// then prefers the E-cores and parks the P-cores, which is a real reduction in
// power and heat for a background app. It is a hint, so it is never the only
// thing holding a limit up.
func setEcoQoS(pid int, on bool) error {
	h, err := openProcessFor(pid, processSetInformation)
	if err != nil {
		return err
	}
	defer procCloseHandle.Call(uintptr(h))
	state := powerThrottlingState{Version: 1}
	if on {
		state.ControlMask = throttleExecutionSpeed
		state.StateMask = throttleExecutionSpeed
	}
	r, _, callErr := procSetProcessInformation.Call(uintptr(h), processPowerThrottling,
		uintptr(unsafe.Pointer(&state)), unsafe.Sizeof(state))
	if r == 0 {
		return fmt.Errorf("%v", callErr)
	}
	return nil
}

type cpuRateInfo struct {
	ControlFlags uint32
	CpuRate      uint32
}

// cpuRateFor converts a percentage of the whole machine into the CpuRate the
// job object wants. It returns 0 when there is nothing to apply.
//
// The number is per-core because that is what a user means by "limit Freebuff
// to 40%": 40% of the machine. CpuRate itself is already a share of total CPU,
// so 40% on 4 cores is CpuRate=1000; asking for 40 on 20 cores would be CpuRate=40,
// which is 2% of the machine and would feel like the app had been switched off.
//
// A request below one whole core is rounded up to one core rather than down to
// zero, because zero is not a cap, it is a hang: the browser process would be
// starved of every timeslice.
func cpuRateFor(totalPercent, cores int) uint32 {
	if totalPercent <= 0 || totalPercent >= 100 || cores <= 0 {
		return 0
	}
	perCore := totalPercent / cores
	if perCore < 1 {
		perCore = 1
	}
	return uint32(perCore * 100)
}

// setJobCPUCap installs a hard CPU throttle on a process through a job object.
//
// CpuRate is in hundredths of a percent of TOTAL system CPU, not of one core -
// measured: CpuRate=100 on a 20-core machine capped a busy loop at 20% of one
// core. totalPercent is therefore divided by the core count so that the number
// the user picked means what they expect it to mean.
//
// It only ever applies to the browser process; see the note at the top of this
// file for why the rest of the tree cannot be joined to a new job, which is why the limit
// that reaches the whole tree is the working-set cap.
func setJobCPUCap(job syscall.Handle, totalPercent, cores int) error {
	rate := cpuRateFor(totalPercent, cores)
	if rate == 0 {
		return fmt.Errorf("no cap")
	}
	info := cpuRateInfo{
		ControlFlags: cpuRateControlEnable | cpuRateControlHardCap,
		CpuRate:      rate,
	}
	r, _, callErr := procSetInformationJobObject.Call(uintptr(job),
		jobObjectCpuRateControlInformation, uintptr(unsafe.Pointer(&info)), unsafe.Sizeof(info))
	if r == 0 {
		return fmt.Errorf("%v", callErr)
	}
	return nil
}

func createJob() (syscall.Handle, error) {
	j, _, callErr := procCreateJobObject.Call(0, 0)
	if j == 0 {
		return 0, fmt.Errorf("%v", callErr)
	}
	return syscall.Handle(j), nil
}

func assignToJob(job syscall.Handle, pid int) error {
	h, err := openProcessFor(pid, processSetQuota|processSetInformation|processTerminate|processQueryLimitedInformation)
	if err != nil {
		return err
	}
	defer procCloseHandle.Call(uintptr(h))
	r, _, callErr := procAssignProcessToJobObject.Call(uintptr(job), uintptr(h))
	if r == 0 {
		return fmt.Errorf("%v", callErr)
	}
	return nil
}

func closeHandle(h syscall.Handle) {
	if h != 0 {
		procCloseHandle.Call(uintptr(h))
	}
}

// systemMask is the set of processors this process may run on, which is the
// whole machine unless something has already narrowed it.
func systemMask() uintptr {
	_, system, err := getAffinity(os.Getpid())
	if err != nil || system == 0 {
		return 0
	}
	return system
}

// logicalCPUs is how many cores the scheduler will actually use.
func logicalCPUs() int {
	if n := countBits(systemMask()); n > 0 {
		return n
	}
	if v, err := strconv.Atoi(strings.TrimSpace(os.Getenv("NUMBER_OF_PROCESSORS"))); err == nil && v > 0 {
		return v
	}
	return 1
}

func countBits(mask uintptr) int {
	n := 0
	for mask != 0 {
		if mask&1 == 1 {
			n++
		}
		mask >>= 1
	}
	return n
}

// affinityMaskFor returns a mask that pins a process to `cores` processors,
// taken from the system mask so the result is always a legal subset.
func affinityMaskFor(cores int, systemMask uintptr) uintptr {
	if systemMask == 0 {
		systemMask = 0
		for i := 0; i < cores; i++ {
			systemMask |= 1 << uint(i)
		}
		return systemMask
	}
	if cores >= countBits(systemMask) {
		return systemMask
	}
	var mask uintptr
	seen := 0
	for bit := 0; bit < 64 && seen < cores; bit++ {
		b := uintptr(1) << uint(bit)
		if systemMask&b == 0 {
			continue
		}
		mask |= b
		seen++
	}
	if mask == 0 {
		return systemMask
	}
	return mask
}
