package main

import (
	"maps"
	"testing"
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
