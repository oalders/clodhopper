package main

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

// procInfo is one process-table entry: its parent pid and, when hasKB, its
// memory in KiB (PSS on Linux, else RSS).
type procInfo struct {
	ppid  int
	kb    int64
	hasKB bool
}

// procTable maps pid → procInfo.
type procTable map[int]procInfo

// children inverts the table's parent links.
func (t procTable) children() map[int][]int {
	kids := map[int][]int{}
	for pid, p := range t {
		kids[p.ppid] = append(kids[p.ppid], pid)
	}
	return kids
}

// descendants returns root and every pid below it; the visited set stops a ppid cycle looping.
func descendants(kids map[int][]int, root int, seen map[int]bool) []int {
	var out []int
	for stack := []int{root}; len(stack) > 0; {
		pid := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if seen[pid] {
			continue
		}
		seen[pid] = true
		out = append(out, pid)
		stack = append(stack, kids[pid]...)
	}
	return out
}

// treeMem sums, per pane, the memory of its root pid's whole process tree. Pids
// with no figure add nothing; a pane with pid 0, a root missing from the table
// or no figure anywhere in its tree is absent. Children a daemon reparented away
// from the tree are not counted, so a figure can only underestimate.
func treeMem(t procTable, roots map[string]int) map[string]int64 {
	kids := t.children()
	out := map[string]int64{}
	for pane, root := range roots {
		if _, ok := t[root]; root <= 0 || !ok {
			continue
		}
		var kb int64
		found := false
		for _, pid := range descendants(kids, root, map[int]bool{}) {
			if p := t[pid]; p.hasKB {
				kb += p.kb
				found = true
			}
		}
		if found {
			out[pane] = kb
		}
	}
	return out
}

// procBudget caps a process-table read, which runs on the render path.
const procBudget = 2 * time.Second

// readProcTable snapshots the processes, with memory only for the roots' trees.
func readProcTable(roots map[string]int) procTable {
	if runtime.GOOS == "linux" {
		return readLinuxProcs(roots)
	}
	return readPSProcs()
}

// readPSProcs snapshots every process via ps, which reports RSS for all of them.
func readPSProcs() procTable {
	ctx, cancel := context.WithTimeout(context.Background(), procBudget)
	defer cancel()
	out, err := exec.CommandContext(ctx, "ps", "-axo", "pid=,ppid=,rss=").Output()
	if err != nil {
		return nil
	}
	return parsePS(out)
}

// parsePS parses `ps -axo pid=,ppid=,rss=` output, skipping malformed lines.
func parsePS(out []byte) procTable {
	t := procTable{}
	for line := range strings.SplitSeq(string(out), "\n") {
		f := strings.Fields(line)
		if len(f) != 3 {
			continue
		}
		pid, err1 := strconv.Atoi(f[0])
		ppid, err2 := strconv.Atoi(f[1])
		kb, err3 := strconv.ParseInt(f[2], 10, 64)
		if err1 != nil || err2 != nil {
			continue
		}
		t[pid] = procInfo{ppid: ppid, kb: kb, hasKB: err3 == nil}
	}
	return t
}

// readLinuxProcs builds the table from /proc: every pid's ppid, then memory for
// the roots' descendants only, so a busy host pays one small read per agent process.
// Past procBudget it stops and returns what it has, which can only underestimate.
func readLinuxProcs(roots map[string]int) procTable {
	deadline := time.Now().Add(procBudget)
	ents, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}
	t := procTable{}
	for _, e := range ents {
		if time.Now().After(deadline) {
			return t
		}
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		stat, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
		if err != nil {
			continue
		}
		if ppid, ok := statPPID(stat); ok {
			t[pid] = procInfo{ppid: ppid}
		}
	}
	kids, seen := t.children(), map[int]bool{}
	for _, root := range roots {
		if _, ok := t[root]; root <= 0 || !ok {
			continue
		}
		for _, pid := range descendants(kids, root, seen) {
			if time.Now().After(deadline) {
				return t
			}
			p := t[pid]
			p.kb, p.hasKB = linuxProcKB(pid)
			t[pid] = p
		}
	}
	return t
}

// linuxProcKB reads pid's PSS, falling back to VmRSS when smaps_rollup is unreadable.
func linuxProcKB(pid int) (int64, bool) {
	if b, err := os.ReadFile(fmt.Sprintf("/proc/%d/smaps_rollup", pid)); err == nil {
		if kb, ok := fieldKB(b, "Pss:"); ok {
			return kb, true
		}
	}
	if b, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid)); err == nil {
		return fieldKB(b, "VmRSS:")
	}
	return 0, false
}

// statPPID extracts the ppid from /proc/<pid>/stat, parsing after the LAST ")"
// because comm may itself hold spaces and parens.
func statPPID(stat []byte) (int, bool) {
	i := bytes.LastIndexByte(stat, ')')
	if i < 0 {
		return 0, false
	}
	f := strings.Fields(string(stat[i+1:]))
	if len(f) < 2 {
		return 0, false
	}
	ppid, err := strconv.Atoi(f[1])
	return ppid, err == nil
}

// fieldKB returns the value of a "Key:   123 kB" line.
func fieldKB(b []byte, key string) (int64, bool) {
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		if rest, ok := strings.CutPrefix(sc.Text(), key); ok {
			f := strings.Fields(rest)
			if len(f) == 0 {
				return 0, false
			}
			kb, err := strconv.ParseInt(f[0], 10, 64)
			return kb, err == nil
		}
	}
	return 0, false
}

// paneMem memoises paneCache.mem under its own lock, so a /proc scan never holds up panes().
type paneMem struct {
	mu   sync.Mutex
	kb   map[string]int64 // nil until the first scan
	at   time.Time
	busy bool // a scan is outstanding; no second one starts
}

// readProcs and memWait are vars so a test can hang a scan and not wait long for it.
var (
	readProcs = readProcTable
	// A margin over procBudget, so a scan stopping at its deadline usually lands in this render.
	memWait = procBudget + 500*time.Millisecond
)

// memStale hides memoised figures this old, so a scan wedged in the kernel never freezes them as current.
const memStale = 12 * paneTTL

// mem returns pane id → KiB of its process tree, refreshed once per paneTTL. It
// is lazy so /api/pane and the action paths, which call panes()/live(), never pay
// for the scan. With no pane pid known it scans nothing. A nil cache returns nil.
// The scan runs in a goroutine, since a read can block in the kernel past
// procBudget (smaps_rollup on a task stuck holding mmap_lock); after memWait the
// render gets the previous figures (nil if none, or older than memStale while a
// wedged scan stays busy) and the late result lands in the memo. No new scan
// starts until a stuck read returns, so until then, once past memStale, figures stay hidden.
// A pane appearing within paneTTL of a scan has no figure until the next scan.
// serve must share the panes' pid namespace, or the figures may be absent or belong to unrelated processes.
func (c *paneCache) mem(now time.Time) map[string]int64 {
	if c == nil {
		return nil
	}
	roots := map[string]int{}
	for id, p := range c.panes(now) {
		if p.pid > 0 {
			roots[id] = p.pid
		}
	}
	m := &c.memo
	// prev must run under m.mu; m.at is a caller's now at scan start, so the age is in callers' clock.
	prev := func() map[string]int64 {
		if now.Sub(m.at) > memStale {
			return nil
		}
		return m.kb
	}
	m.mu.Lock()
	if m.busy || (m.kb != nil && now.Sub(m.at) < paneTTL) {
		defer m.mu.Unlock()
		return prev()
	}
	m.busy = true
	m.mu.Unlock()
	done, read := make(chan map[string]int64, 1), readProcs
	go func() {
		kb := map[string]int64{}
		if len(roots) > 0 {
			kb = treeMem(read(roots), roots)
		}
		m.mu.Lock()
		m.kb, m.at, m.busy = kb, now, false
		m.mu.Unlock()
		done <- kb
	}()
	select {
	case kb := <-done:
		return kb
	case <-time.After(memWait):
		m.mu.Lock()
		defer m.mu.Unlock()
		return prev()
	}
}

// memSize renders KiB as whole MB under 1 GiB, else GB to one decimal (1024-based).
func memSize(kb int64) string {
	if kb < 1<<20 {
		return fmt.Sprintf("%d MB", kb>>10)
	}
	return fmt.Sprintf("%.1f GB", float64(kb)/(1<<20))
}
