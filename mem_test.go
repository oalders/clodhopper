package main

import (
	"maps"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// treeMem sums each pane's whole process tree, skipping pids without a figure,
// leaving out a pane with no usable root, and surviving a ppid cycle.
func TestTreeMem(t *testing.T) {
	table := procTable{
		10: {ppid: 1, kb: 100, hasKB: true}, // pane %1's shell
		11: {ppid: 10, kb: 200, hasKB: true},
		12: {ppid: 11, kb: 300, hasKB: true}, // grandchild
		13: {ppid: 10},                       // unreadable: adds nothing
		20: {ppid: 1},                        // pane %2: no figure anywhere
		30: {ppid: 31, kb: 5, hasKB: true},   // pane %3: a ppid cycle
		31: {ppid: 30, kb: 7, hasKB: true},
		99: {ppid: 1, kb: 1000, hasKB: true}, // unrelated process
	}
	roots := map[string]int{"%1": 10, "%2": 20, "%3": 30, "%4": 40, "%5": 0}
	want := map[string]int64{"%1": 600, "%3": 12}
	if got := treeMem(table, roots); !maps.Equal(got, want) {
		t.Fatalf("treeMem = %v, want %v", got, want)
	}
}

func TestStatPPID(t *testing.T) {
	for _, tc := range []struct {
		stat string
		ppid int
		ok   bool
	}{
		{"42 (claude) S 7 42 42 0", 7, true},
		{"42 (a) b (c)) R 9 1 1", 9, true}, // comm holding spaces and ")"
		{"42 (trunc", 0, false},
		{"42 (x) S", 0, false},
	} {
		if ppid, ok := statPPID([]byte(tc.stat)); ppid != tc.ppid || ok != tc.ok {
			t.Errorf("statPPID(%q) = %d, %v, want %d, %v", tc.stat, ppid, ok, tc.ppid, tc.ok)
		}
	}
}

func TestFieldKB(t *testing.T) {
	rollup := "55d0-7ff [rollup]\nRss:      9000 kB\nPss:      4321 kB\nPss_Anon: 100 kB\n"
	if kb, ok := fieldKB([]byte(rollup), "Pss:"); kb != 4321 || !ok {
		t.Errorf("Pss = %d, %v, want 4321", kb, ok)
	}
	status := "Name:\tclaude\nVmRSS:\t  2048 kB\n"
	if kb, ok := fieldKB([]byte(status), "VmRSS:"); kb != 2048 || !ok {
		t.Errorf("VmRSS = %d, %v, want 2048", kb, ok)
	}
	// A kernel thread's status has no VmRSS line.
	if _, ok := fieldKB([]byte("Name:\tkthreadd\n"), "VmRSS:"); ok {
		t.Error("missing VmRSS line must yield no figure")
	}
}

func TestParsePS(t *testing.T) {
	out := "    1     0  1200\n  500     1 34567\n  501   500     -\nbogus line\n"
	want := procTable{1: {ppid: 0, kb: 1200, hasKB: true}, 500: {ppid: 1, kb: 34567, hasKB: true}, 501: {ppid: 500}}
	if got := parsePS([]byte(out)); !maps.Equal(got, want) {
		t.Fatalf("parsePS = %v, want %v", got, want)
	}
}

// readPSProcs parses the table from ps, and a failing ps yields none.
func TestReadPSProcs(t *testing.T) {
	stub := writeStub(t, "ps", `[ "$*" = "-axo pid=,ppid=,rss=" ] || exit 2
echo "    1     0  1200"
echo "  500     1 34567"`)
	t.Setenv("PATH", filepath.Dir(stub)+string(os.PathListSeparator)+os.Getenv("PATH"))
	want := procTable{1: {ppid: 0, kb: 1200, hasKB: true}, 500: {ppid: 1, kb: 34567, hasKB: true}}
	if got := readPSProcs(); !maps.Equal(got, want) {
		t.Fatalf("readPSProcs = %v, want %v", got, want)
	}
	stub = writeStub(t, "ps", "exit 1")
	t.Setenv("PATH", filepath.Dir(stub)+string(os.PathListSeparator)+os.Getenv("PATH"))
	if got := readPSProcs(); got != nil {
		t.Errorf("failing ps = %v, want nil", got)
	}
}

func TestMemSize(t *testing.T) {
	for kb, want := range map[int64]string{
		412 * 1024:    "412 MB",
		1<<20 - 1:     "1023 MB",
		1 << 20:       "1.0 GB",
		61 << 20 / 10: "6.1 GB",
	} {
		if got := memSize(kb); got != want {
			t.Errorf("memSize(%d) = %q, want %q", kb, got, want)
		}
	}
}

// The real /proc reader finds this test's own process and its memory.
func TestReadLinuxProcsSelf(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("reads /proc")
	}
	pid := os.Getpid()
	if p := readLinuxProcs(map[string]int{"%1": pid})[pid]; !p.hasKB || p.kb <= 0 {
		t.Fatalf("own pid %d = %+v, want a positive figure", pid, p)
	}
}

// mem scans once, serves its memo within paneTTL, and rescans after it. The
// pane listing is restamped each step so panes() never runs real tmux.
func TestPaneCacheMemMemo(t *testing.T) {
	calls := 0
	oldRead := readProcs
	readProcs = func(map[string]int) procTable {
		calls++
		return procTable{7: {kb: int64(calls), hasKB: true}}
	}
	t.Cleanup(func() { readProcs = oldRead })
	now := time.Now()
	c := &paneCache{set: map[string]livePane{"%1": {pid: 7}}, at: now}
	if kb := c.mem(now)["%1"]; kb != 1 {
		t.Fatalf("first scan = %d, want 1", kb)
	}
	if kb := c.mem(now.Add(paneTTL - time.Second))["%1"]; kb != 1 {
		t.Errorf("within paneTTL = %d, want the memoised 1", kb)
	}
	later := now.Add(paneTTL)
	c.at = later
	if kb := c.mem(later)["%1"]; kb != 2 {
		t.Errorf("after paneTTL = %d, want the rescan's 2", kb)
	}
}

func TestPaneCacheMemNil(t *testing.T) {
	var c *paneCache
	if m := c.mem(time.Now()); m != nil {
		t.Errorf("nil cache mem = %v, want nil", m)
	}
}

// A hung scan does not stall renders: mem serves the stale memo after memWait,
// starts no second scan while one is outstanding, hides the memo once it is
// older than memStale, and the late result lands.
func TestPaneCacheMemHungScan(t *testing.T) {
	release, calls := make(chan struct{}), atomic.Int32{}
	oldRead, oldWait := readProcs, memWait
	readProcs = func(map[string]int) procTable {
		calls.Add(1)
		<-release
		return procTable{7: {kb: 99, hasKB: true}}
	}
	memWait = 20 * time.Millisecond
	t.Cleanup(func() { readProcs, memWait = oldRead, oldWait })
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	t.Cleanup(unblock) // a failing run must not leave the scan goroutine parked

	now := time.Now()
	c := &paneCache{set: map[string]livePane{"%1": {pid: 7}}, at: now.Add(time.Hour)}
	stamp := now.Add(-paneTTL)
	c.memo = paneMem{kb: map[string]int64{"%1": 1}, at: stamp}
	for i := range 2 {
		start := time.Now()
		if kb := c.mem(now)["%1"]; kb != 1 {
			t.Errorf("call %d during hung scan = %d, want the stale 1", i, kb)
		}
		if d := time.Since(start); d > time.Second {
			t.Errorf("call %d took %v, want about memWait", i, d)
		}
	}
	if kb := c.mem(stamp.Add(memStale))["%1"]; kb != 1 {
		t.Errorf("at memStale during hung scan = %d, want the stale 1", kb)
	}
	if kb := c.mem(stamp.Add(memStale + time.Second)); kb != nil {
		t.Errorf("past memStale during hung scan = %v, want nil", kb)
	}
	if n := calls.Load(); n != 1 {
		t.Errorf("scans started while hung = %d, want 1", n)
	}
	unblock()
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(5 * time.Millisecond) {
		c.memo.mu.Lock()
		kb, busy := c.memo.kb["%1"], c.memo.busy
		c.memo.mu.Unlock()
		if kb == 99 && !busy {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("late result never landed: kb=%d busy=%t", kb, busy)
		}
	}
	if n := calls.Load(); n != 1 {
		t.Errorf("scans started = %d, want 1", n)
	}
}
