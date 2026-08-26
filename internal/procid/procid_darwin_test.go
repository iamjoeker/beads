//go:build darwin

package procid

import (
	"errors"
	"os/exec"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestTokenFromKinfoProc(t *testing.T) {
	tests := []struct {
		name     string
		stat     int8
		wantTok  Token
		wantGone bool
	}{
		{name: "running", stat: 2 /* SRUN */, wantTok: "darwin-v1:1.2"},
		{name: "sleeping", stat: 3 /* SSLEEP */, wantTok: "darwin-v1:1.2"},
		{name: "zombie", stat: statZombie, wantGone: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var proc unix.KinfoProc
			proc.Proc.P_stat = tt.stat
			proc.Proc.P_starttime.Sec = 1
			proc.Proc.P_starttime.Usec = 2

			got, err := tokenFromKinfoProc(4321, &proc)
			if tt.wantGone {
				if err == nil {
					t.Fatalf("tokenFromKinfoProc(zombie) = %q, want error", got)
				}
				if !IsProcessGone(err) {
					t.Fatalf("IsProcessGone(%v) = false, want true", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("tokenFromKinfoProc: %v", err)
			}
			if got != tt.wantTok {
				t.Errorf("tokenFromKinfoProc = %q, want %q", got, tt.wantTok)
			}
		})
	}
}

// The darwin counterpart of the linux TestVerifyZombieChildIsGone.
// kern.proc.pid keeps answering for an unreaped child, and answers with its
// original P_starttime, so Verify used to match the token and report a dead
// process as alive.
func TestVerifyZombieChildIsGone(t *testing.T) {
	cmd := exec.Command("sleep", "0.2")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start child: %v", err)
	}
	tok, err := Capture(cmd.Process.Pid)
	if err != nil {
		_ = cmd.Wait()
		t.Fatalf("Capture(child): %v", err)
	}
	defer func() { _ = cmd.Wait() }()

	deadline := time.Now().Add(5 * time.Second)
	for {
		matched, verifyErr := Verify(cmd.Process.Pid, tok)
		if verifyErr != nil {
			t.Fatalf("Verify(zombie child): %v", verifyErr)
		}
		if !matched {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("Verify(zombie child) remained true")
		}
		time.Sleep(10 * time.Millisecond)
	}

	// Signal 0 still sees an unreaped zombie's PID, proving Verify classified
	// process state rather than relying only on the PID disappearing.
	if err := syscall.Kill(cmd.Process.Pid, 0); err != nil && !errors.Is(err, unix.EPERM) {
		t.Fatalf("signal 0 to unreaped zombie: %v", err)
	}
}
