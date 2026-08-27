//go:build linux

package agent

import (
	"bufio"
	"os"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

// capSysNice is CAP_SYS_NICE's bit position in the capability bitmask (linux/capability.h).
const capSysNice = 23

// SchedPrioAvailable reports whether this process can actually put a worker on SCHED_FIFO.
//
// It is a capability in the §10.2 sense — the server would make a wrong decision without it,
// because a request asking for sched_prio on a node that cannot honour it must fail at request
// time rather than produce workers that silently run at normal priority, which looks like a
// performance problem in the media rather than a configuration problem in the request.
//
// Note what does *not* go over the wire: CAP_SYS_NICE and RLIMIT_RTPRIO themselves. Raw kernel
// capabilities gate whether the agent advertises something, and the server never has to reason
// about them (§10.2).
//
// Either mechanism is sufficient, and both are checked because a container commonly has one
// without the other: CAP_SYS_NICE lets a process raise priority unconditionally, while a
// non-zero RLIMIT_RTPRIO lets an unprivileged one do it up to that ceiling. The worker's
// sched_setscheduler failing is fatal *after* the connection is established, with no graceful
// degradation (WRS §9), so guessing here is expensive.
func SchedPrioAvailable() bool {
	var limit unix.Rlimit
	if err := unix.Getrlimit(unix.RLIMIT_RTPRIO, &limit); err == nil && limit.Cur > 0 {
		return true
	}
	return hasCapSysNice()
}

// hasCapSysNice reads the effective capability set from /proc/self/status.
//
// Reading proc rather than calling capget: the value is a hex bitmask on one line, this runs
// once per registration, and it avoids a second way of asking the kernel the same question.
func hasCapSysNice() bool {
	file, err := os.Open("/proc/self/status")
	if err != nil {
		return false
	}
	defer func() { _ = file.Close() }()

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		value, ok := strings.CutPrefix(scanner.Text(), "CapEff:")
		if !ok {
			continue
		}
		mask, err := strconv.ParseUint(strings.TrimSpace(value), 16, 64)
		if err != nil {
			return false
		}
		return mask&(1<<capSysNice) != 0
	}
	return false
}
