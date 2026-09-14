//go:build cgo
// +build cgo

package pg_query_test

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	pg_query "github.com/hellower/pg_query_go/v6"
)

// goosedb fork: Normalize must either replace every constant or return an
// error. Before v6.2.2-goosedb.5 the stack-depth error raised inside the
// normalize walker was swallowed, so deep input came back "successfully" with
// only part of its constants replaced; a long UNION chain never reached a
// stack check and killed the process; and an error raised after a sibling
// clause had been walked longjmp'ed into a stack frame that had already
// returned.
//
// Each case runs in a child process under a deadline and a memory cap. The
// regressions this guards against are a crash and an allocation loop (the
// dead-frame case grew past 60GB of RSS within ten seconds when measured), so
// neither may run in the test binary itself or be left to exhaust the host.

const (
	normalizeChildEnv = "PG_QUERY_GO_NORMALIZE_STACK_DEPTH_CASE"
	// Every case peaks below 100MB of RSS when the walker behaves (measured
	// on darwin/arm64).
	normalizeChildMaxRSS   = 1 << 30
	normalizeChildDeadline = 60 * time.Second
)

func normalizeStackDepthSQL(head, tail string, n int) string {
	return head + strings.Repeat(tail, n)
}

type normalizeStackDepthArgs struct {
	sql string
}

var normalizeStackDepthTests = []struct {
	name string
	args normalizeStackDepthArgs
	// want is the number of $n placeholders in the result: every constant in
	// the input. Ignored when wantErr is set.
	want    int
	wantErr string
}{
	{
		name: "single constant",
		args: normalizeStackDepthArgs{sql: "SELECT 1"},
		want: 1,
	},
	{
		name: "shallow expression chain is fully normalized",
		args: normalizeStackDepthArgs{sql: normalizeStackDepthSQL("SELECT 1", "+1", 100)},
		want: 101,
	},
	{
		name: "shallow UNION chain is fully normalized",
		args: normalizeStackDepthArgs{sql: normalizeStackDepthSQL("SELECT 1", " UNION ALL SELECT 1", 200)},
		want: 201,
	},
	{
		// OR lists are flattened by the grammar, so this is large but only a
		// few levels deep: the guard must not reject it.
		name: "wide but shallow OR list is fully normalized",
		args: normalizeStackDepthArgs{sql: normalizeStackDepthSQL("SELECT x FROM t WHERE x = 1", " OR x = 1", 19999)},
		want: 20000,
	},
	{
		// Without the re-throw in the walker's PG_CATCH this returns a
		// partially normalized query and no error.
		name:    "deep expression chain is rejected, not partially normalized",
		args:    normalizeStackDepthArgs{sql: normalizeStackDepthSQL("SELECT 1", "+1", 250000)},
		wantErr: "stack depth limit exceeded",
	},
	{
		// SelectStmt recurses into the walker directly; without the check at
		// the walker's entry this overflows the thread stack.
		name:    "deep UNION chain is rejected",
		args:    normalizeStackDepthArgs{sql: normalizeStackDepthSQL("SELECT 1", " UNION ALL SELECT 1", 100000)},
		wantErr: "stack depth limit exceeded",
	},
	{
		// The first target is walked to completion before the deep one. If
		// the walker returns from inside PG_TRY, that completed walk leaves
		// PG_exception_stack pointing at its dead frame, and the error from
		// the deep target jumps there (measured: an allocation loop).
		name:    "deep expression after a sibling target is rejected",
		args:    normalizeStackDepthArgs{sql: normalizeStackDepthSQL("SELECT 1, 1", "+1", 250000)},
		wantErr: "stack depth limit exceeded",
	},
	{
		name:    "deep expression after a WHERE constant is rejected",
		args:    normalizeStackDepthArgs{sql: normalizeStackDepthSQL("SELECT 1 FROM t WHERE x = 1", "+1", 250000)},
		wantErr: "stack depth limit exceeded",
	},
}

func TestNormalizeStackDepth(t *testing.T) {
	if idx := os.Getenv(normalizeChildEnv); idx != "" {
		// Child: run one case and report on stdout.
		i, err := strconv.Atoi(idx)
		if err != nil || i < 0 || i >= len(normalizeStackDepthTests) {
			t.Fatalf("bad %s=%q", normalizeChildEnv, idx)
		}
		got, err := pg_query.Normalize(normalizeStackDepthTests[i].args.sql)
		if err != nil {
			fmt.Printf("RESULT err %d %s\n", len(got), err.Error())
		} else {
			fmt.Printf("RESULT ok %d %d\n", len(got), strings.Count(got, "$"))
		}
		return
	}

	for i, tt := range normalizeStackDepthTests {
		t.Run(tt.name, func(t *testing.T) {
			out, runErr := runNormalizeChild(t, i)

			var line string
			for _, l := range strings.Split(out, "\n") {
				if strings.HasPrefix(l, "RESULT ") {
					line = strings.TrimPrefix(l, "RESULT ")
					break
				}
			}
			if line == "" {
				t.Fatalf("Normalize() did not return (child exit: %v)\n%s", runErr, lastLines(out, 5))
			}

			fields := strings.SplitN(line, " ", 3)
			if len(fields) != 3 {
				t.Fatalf("malformed child report %q", line)
			}
			gotLen, _ := strconv.Atoi(fields[1])
			if tt.wantErr != "" {
				if fields[0] != "err" {
					t.Fatalf("Normalize() error = nil, want %q; got %s placeholders in a %d-byte result",
						tt.wantErr, fields[2], gotLen)
				}
				if !strings.Contains(fields[2], tt.wantErr) {
					t.Fatalf("Normalize() error = %q, want it to contain %q", fields[2], tt.wantErr)
				}
				if gotLen != 0 {
					t.Fatalf("Normalize() returned a %d-byte result alongside the error", gotLen)
				}
				return
			}
			if fields[0] != "ok" {
				t.Fatalf("Normalize() unexpected error: %s", fields[2])
			}
			if n, _ := strconv.Atoi(fields[2]); n != tt.want {
				t.Fatalf("Normalize() placeholders = %d, want %d", n, tt.want)
			}
		})
	}
}

// runNormalizeChild runs case i in a child process and kills it if it outlives
// the deadline or its RSS passes the cap.
func runNormalizeChild(t *testing.T, i int) (string, error) {
	t.Helper()
	var out bytes.Buffer
	cmd := exec.Command(os.Args[0], "-test.run=^TestNormalizeStackDepth$")
	cmd.Env = append(os.Environ(), fmt.Sprintf("%s=%d", normalizeChildEnv, i))
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Start(); err != nil {
		t.Fatalf("start child: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	deadline := time.After(normalizeChildDeadline)
	tick := time.NewTicker(20 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case err := <-done:
			return out.String(), err
		case <-deadline:
			_ = cmd.Process.Kill()
			<-done
			t.Fatalf("Normalize() did not return within %s (child killed)", normalizeChildDeadline)
		case <-tick.C:
			if rss, ok := processRSS(cmd.Process.Pid); ok && rss > normalizeChildMaxRSS {
				_ = cmd.Process.Kill()
				<-done
				t.Fatalf("Normalize() child passed %dMB of RSS (killed)", normalizeChildMaxRSS>>20)
			}
		}
	}
}

// processRSS reports a process's resident set size in bytes. ok is false when
// it cannot be read, e.g. the process has already exited.
func processRSS(pid int) (rss int64, ok bool) {
	if runtime.GOOS == "linux" {
		// /proc is always there; ps is not in every container image.
		b, err := os.ReadFile(fmt.Sprintf("/proc/%d/statm", pid))
		if err != nil {
			return 0, false
		}
		f := strings.Fields(string(b))
		if len(f) < 2 {
			return 0, false
		}
		pages, err := strconv.ParseInt(f[1], 10, 64)
		if err != nil {
			return 0, false
		}
		return pages * int64(os.Getpagesize()), true
	}
	b, err := exec.Command("ps", "-o", "rss=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return 0, false
	}
	kb, err := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
	if err != nil {
		return 0, false
	}
	return kb << 10, true
}

func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}
