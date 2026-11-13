package filelock

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// increment does a read-modify-write of a counter file under the lock.
func increment(t testing.TB, lockPath, counter string) {
	l, err := AcquireTimeout(lockPath, 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Unlock()
	data, _ := os.ReadFile(counter)
	n, _ := strconv.Atoi(strings.TrimSpace(string(data)))
	// Widen the race window so a broken lock would lose updates.
	time.Sleep(100 * time.Microsecond)
	if err := os.WriteFile(counter, []byte(strconv.Itoa(n+1)), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestHelperProcess(t *testing.T) {
	if os.Getenv("FILELOCK_HELPER") != "1" {
		t.Skip("helper process only")
	}
	n, _ := strconv.Atoi(os.Getenv("FILELOCK_N"))
	for i := 0; i < n; i++ {
		increment(t, os.Getenv("FILELOCK_LOCK"), os.Getenv("FILELOCK_COUNTER"))
	}
}

func TestExclusiveAcrossProcesses(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns processes")
	}
	dir := t.TempDir()
	lockPath, counter := filepath.Join(dir, "x.lock"), filepath.Join(dir, "count")
	const procs, each = 4, 40
	var cmds []*exec.Cmd
	for i := 0; i < procs; i++ {
		cmd := exec.Command(os.Args[0], "-test.run=^TestHelperProcess$")
		cmd.Env = append(os.Environ(), "FILELOCK_HELPER=1", "FILELOCK_N="+strconv.Itoa(each),
			"FILELOCK_LOCK="+lockPath, "FILELOCK_COUNTER="+counter)
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		cmds = append(cmds, cmd)
	}
	// This process competes too, from several goroutines.
	var wg sync.WaitGroup
	for g := 0; g < 4; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < each; i++ {
				increment(t, lockPath, counter)
			}
		}()
	}
	wg.Wait()
	for _, c := range cmds {
		if err := c.Wait(); err != nil {
			t.Fatal(err)
		}
	}
	data, _ := os.ReadFile(counter)
	if got := strings.TrimSpace(string(data)); got != strconv.Itoa((procs+4)*each) {
		t.Fatalf("counter = %s, want %d (lost updates)", got, (procs+4)*each)
	}
	if fi, err := os.Stat(lockPath); err != nil || fi.Mode().Perm()&0o077 != 0 {
		t.Fatalf("lock file mode: %v %v", fi.Mode(), err)
	}
}

func TestTimeoutAndCancel(t *testing.T) {
	p := filepath.Join(t.TempDir(), "sub", "a.lock")
	l, err := AcquireTimeout(p, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if l.Path() != p {
		t.Fatal("path")
	}
	if _, err := AcquireTimeout(p, 30*time.Millisecond); !errors.Is(err, ErrTimeout) {
		t.Fatalf("want timeout, got %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Acquire(ctx, p); !errors.Is(err, context.Canceled) {
		t.Fatalf("want canceled, got %v", err)
	}
	if err := l.Unlock(); err != nil {
		t.Fatal(err)
	}
	if err := l.Unlock(); err != nil {
		t.Fatal("second unlock should be a no-op")
	}
	l2, err := AcquireTimeout(p, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	l2.Unlock()
}
