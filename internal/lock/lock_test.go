//go:build unix

package lock

import (
	"bufio"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestMain lets the test binary act as a second process holding the lock.
func TestMain(m *testing.M) {
	if path := os.Getenv("CALBRIDGE_LOCK_HELPER"); path != "" {
		l, err := Acquire(path)
		if err != nil {
			os.Stdout.WriteString("error " + err.Error() + "\n")
			os.Exit(3)
		}
		os.Stdout.WriteString("locked\n")
		_, _ = bufio.NewReader(os.Stdin).ReadString('\n') // hold until stdin closes
		_ = l.Release()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func TestSecondProcessIsRefused(t *testing.T) {
	path := filepath.Join(t.TempDir(), "calbridge.db.lock")
	cmd := exec.Command(os.Args[0], "-test.run=^$") // #nosec G204 -- the test binary itself
	cmd.Env = append(os.Environ(), "CALBRIDGE_LOCK_HELPER="+path)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	line, err := bufio.NewReader(stdout).ReadString('\n')
	if err != nil || strings.TrimSpace(line) != "locked" {
		t.Fatalf("helper: %q %v", line, err)
	}

	if _, err := Acquire(path); !errors.Is(err, ErrLocked) {
		t.Fatalf("Acquire while held by another process = %v; want ErrLocked", err)
	}

	_ = stdin.Close() // helper releases and exits
	if err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		l, err := Acquire(path)
		if err == nil {
			_ = l.Release()
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("lock not released after the holder exited: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestReleaseAndReacquire(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x", "db.lock")
	l, err := Acquire(path)
	if err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(path)
	if err != nil || fi.Mode().Perm()&0o077 != 0 {
		t.Fatalf("lock file mode %v %v", fi.Mode(), err)
	}
	if err := l.Release(); err != nil {
		t.Fatal(err)
	}
	l2, err := Acquire(path)
	if err != nil {
		t.Fatalf("reacquire: %v", err)
	}
	_ = l2.Release()
	if PathFor("/var/lib/calbridge/calbridge.db") != "/var/lib/calbridge/calbridge.db.lock" {
		t.Error("PathFor")
	}
}
