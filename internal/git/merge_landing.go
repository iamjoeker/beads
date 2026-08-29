package git

import (
	"bytes"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// runMergeLandingGit runs git in dir and returns trimmed stdout, folding
// stderr into the error for diagnosability. It is deliberately separate from
// the package's other exec paths: every caller here treats failure as
// "unprovable" rather than fatal, so callers only need the message, never a
// typed error.
func runMergeLandingGit(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return "", fmt.Errorf("git %s: %s", strings.Join(args, " "), msg)
	}
	return strings.TrimSpace(stdout.String()), nil
}

// remoteDefaultBranch best-effort resolves origin's default branch name in
// dir. Returns "" when it cannot be determined.
func remoteDefaultBranch(dir string) string {
	if out, err := runMergeLandingGit(dir, "symbolic-ref", "refs/remotes/origin/HEAD"); err == nil && out != "" {
		parts := strings.Split(out, "/")
		if len(parts) > 0 && parts[len(parts)-1] != "" {
			return parts[len(parts)-1]
		}
	}
	if _, err := runMergeLandingGit(dir, "rev-parse", "--verify", "origin/main"); err == nil {
		return "main"
	}
	if _, err := runMergeLandingGit(dir, "rev-parse", "--verify", "origin/master"); err == nil {
		return "master"
	}
	return ""
}

// isAncestor reports whether ancestor is reachable from descendant. provable
// is false when git could not answer the question at all (as opposed to
// answering "no").
func isAncestor(dir, ancestor, descendant string) (yes bool, provable bool) {
	cmd := exec.Command("git", "merge-base", "--is-ancestor", ancestor, descendant)
	cmd.Dir = dir
	err := cmd.Run()
	if err == nil {
		return true, true
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
		return false, true
	}
	return false, false
}

// MergeLandingCheck is the result of VerifyHeadLandedOnOrigin.
type MergeLandingCheck struct {
	// Landed is true when HEAD is provably on the remote's default branch, OR
	// when the question could not be answered (fails open — see below).
	Landed bool
	// Target is the remote default branch name that was checked against, or
	// "" if it could not be determined.
	Target string
	// HeadSHA is dir's current HEAD, or "" if it could not be resolved.
	HeadSHA string
}

// VerifyHeadLandedOnOrigin checks whether dir's current HEAD has landed on
// origin's default branch, for the narrow purpose of `bd close` refusing a
// close reason that claims the fix is already merged ("Fixed: ...", "Merged
// in <sha>") when it plainly is not (bd-rpg, reported via gastown's gt-20la):
// a polecat closed a bead with exactly that kind of reason while the fix
// commit still lived only on its own branch — never submitted to the merge
// queue, never reached the target branch.
//
// It fails OPEN — Landed reports true — whenever the question cannot be
// answered: dir is not a git repository, HEAD does not resolve, the remote
// has no discoverable default branch, or fetching the target ref fails
// (offline, unreachable remote, sandboxed environment with no network).
// Absence of proof is not proof of absence, and refusing here would block a
// great many closes that have nothing to do with the defect being guarded
// against.
//
// This checks sha ancestry only: a commit whose CONTENT landed but whose sha
// changed (e.g. a queue rebase) reads as not-landed here. That is narrower
// than a full content-aware merge proof, but sufficient for the failure mode
// this guards against — a close reason asserting landing for work that was
// never pushed anywhere at all.
func VerifyHeadLandedOnOrigin(dir string) MergeLandingCheck {
	if dir == "" {
		dir = "."
	}
	if _, err := runMergeLandingGit(dir, "rev-parse", "--git-dir"); err != nil {
		return MergeLandingCheck{Landed: true}
	}
	head, err := runMergeLandingGit(dir, "rev-parse", "HEAD")
	if err != nil || head == "" {
		return MergeLandingCheck{Landed: true}
	}
	target := remoteDefaultBranch(dir)
	if target == "" {
		return MergeLandingCheck{Landed: true, HeadSHA: head}
	}

	// Fetch into a private, uniquely named ref rather than FETCH_HEAD: that
	// file is per-repository and a concurrent fetch elsewhere in the same
	// gitdir can overwrite it between the fetch and the read.
	tmpRef := fmt.Sprintf("refs/bd-close-verify/%d", time.Now().UnixNano())
	if _, err := runMergeLandingGit(dir, "fetch", "--quiet", "origin", "refs/heads/"+target+":"+tmpRef); err != nil {
		return MergeLandingCheck{Landed: true, Target: target, HeadSHA: head}
	}
	defer func() { _, _ = runMergeLandingGit(dir, "update-ref", "-d", tmpRef) }()

	landed, provable := isAncestor(dir, head, tmpRef)
	if !provable {
		return MergeLandingCheck{Landed: true, Target: target, HeadSHA: head}
	}
	return MergeLandingCheck{Landed: landed, Target: target, HeadSHA: head}
}
