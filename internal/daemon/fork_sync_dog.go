package daemon

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/rig"
)

const (
	defaultForkSyncDogInterval = 1 * time.Hour
)

// ForkSyncDogConfig holds configuration for the fork_sync_dog patrol.
type ForkSyncDogConfig struct {
	// Enabled controls whether the fork sync dog runs.
	Enabled bool `json:"enabled"`

	// IntervalStr is how often to run, as a string (e.g., "1h").
	IntervalStr string `json:"interval,omitempty"`
}

// forkSyncDogInterval returns the configured interval, or the default (1h).
func forkSyncDogInterval(config *DaemonPatrolConfig) time.Duration {
	if config != nil && config.Patrols != nil && config.Patrols.ForkSyncDog != nil {
		if config.Patrols.ForkSyncDog.IntervalStr != "" {
			if d, err := time.ParseDuration(config.Patrols.ForkSyncDog.IntervalStr); err == nil && d > 0 {
				return d
			}
		}
	}
	return defaultForkSyncDogInterval
}

// runForkSyncDog syncs fork/main with origin/main for all rigs that have a fork remote.
// This is the "safety net" (Option B) complementing the Makefile sync-fork target.
func (d *Daemon) runForkSyncDog() {
	if !IsPatrolEnabled(d.patrolConfig, "fork_sync_dog") {
		return
	}

	d.logger.Printf("fork_sync_dog: starting sync cycle")

	mol := d.pourDogMolecule(constants.MolDogForkSync, nil)
	defer mol.close()

	rigs := d.getKnownRigs()
	synced := 0
	skipped := 0
	failed := 0

	for _, rigName := range rigs {
		result := d.syncForkForRig(rigName)
		switch result {
		case forkSyncSynced:
			synced++
		case forkSyncSkipped:
			skipped++
		case forkSyncFailed:
			failed++
		}
	}

	mol.closeStep("sync")

	d.logger.Printf("fork_sync_dog: cycle complete — synced %d, skipped %d, failed %d",
		synced, skipped, failed)
	mol.closeStep("report")
}

type forkSyncResult int

const (
	forkSyncSkipped forkSyncResult = iota
	forkSyncSynced
	forkSyncFailed
)

// syncForkForRig syncs fork/main with origin/main for a single rig.
func (d *Daemon) syncForkForRig(rigName string) forkSyncResult {
	rigPath := filepath.Join(d.config.TownRoot, rigName)

	// Find a workdir for this rig's git operations.
	// Use the refinery clone if it exists, otherwise any worktree.
	workDir := d.findRigWorkDir(rigName)
	if workDir == "" {
		return forkSyncSkipped
	}

	// Check if this rig uses a fork workflow by checking for a 'fork' remote
	remoteOut, err := runGitCmd(workDir, "remote")
	if err != nil {
		return forkSyncSkipped
	}
	hasFork := false
	for _, line := range strings.Split(strings.TrimSpace(remoteOut), "\n") {
		if strings.TrimSpace(line) == "fork" {
			hasFork = true
			break
		}
	}
	if !hasFork {
		// Also check rig config for push_url (indicates fork even if remote isn't named 'fork')
		if cfg, err := rig.LoadRigConfig(rigPath); err != nil || cfg.PushURL == "" {
			return forkSyncSkipped
		}
	}

	// Fetch origin (upstream) and fork
	if _, err := runGitCmd(workDir, "fetch", "origin", "main", "--quiet"); err != nil {
		d.logger.Printf("fork_sync_dog: fetch origin failed for %s: %v", rigName, err)
		return forkSyncFailed
	}
	if _, err := runGitCmd(workDir, "fetch", "fork", "main", "--quiet"); err != nil {
		d.logger.Printf("fork_sync_dog: fetch fork failed for %s: %v", rigName, err)
		return forkSyncFailed
	}

	// Compare refs
	originMain, err := runGitCmd(workDir, "rev-parse", "origin/main")
	if err != nil {
		d.logger.Printf("fork_sync_dog: rev-parse origin/main failed for %s: %v", rigName, err)
		return forkSyncFailed
	}
	originMain = strings.TrimSpace(originMain)

	forkMain, err := runGitCmd(workDir, "rev-parse", "fork/main")
	if err != nil {
		d.logger.Printf("fork_sync_dog: rev-parse fork/main failed for %s: %v", rigName, err)
		return forkSyncFailed
	}
	forkMain = strings.TrimSpace(forkMain)

	// Already in sync
	if originMain == forkMain {
		d.logger.Printf("fork_sync_dog: %s fork already up to date", rigName)
		return forkSyncSkipped
	}

	// Check ancestry: is fork/main an ancestor of origin/main? (fast-forward case)
	if _, err := runGitCmd(workDir, "merge-base", "--is-ancestor", forkMain, originMain); err == nil {
		// Fork is behind upstream — fast-forward push
		d.logger.Printf("fork_sync_dog: %s fork is behind upstream, fast-forwarding", rigName)
		if _, err := runGitCmd(workDir, "push", "fork", "origin/main:main"); err != nil {
			d.logger.Printf("fork_sync_dog: push fast-forward failed for %s: %v", rigName, err)
			return forkSyncFailed
		}
		d.logger.Printf("fork_sync_dog: %s fork synced (fast-forward)", rigName)
		return forkSyncSynced
	}

	// Check if fork is ahead (origin/main is ancestor of fork/main)
	if _, err := runGitCmd(workDir, "merge-base", "--is-ancestor", originMain, forkMain); err == nil {
		d.logger.Printf("fork_sync_dog: %s fork is ahead of upstream (local commits pending). Skipping.", rigName)
		return forkSyncSkipped
	}

	// Diverged — need rebase via temporary worktree
	return d.rebaseForkDiverged(workDir, rigName, originMain, forkMain)
}

// rebaseForkDiverged handles the case where fork/main and origin/main have diverged.
// Creates a temporary worktree, rebases, and pushes with --force-with-lease.
func (d *Daemon) rebaseForkDiverged(workDir, rigName, originMain, forkMain string) forkSyncResult {
	d.logger.Printf("fork_sync_dog: %s fork and upstream diverged, attempting rebase", rigName)

	// Find merge base
	mergeBase, err := runGitCmd(workDir, "merge-base", forkMain, originMain)
	if err != nil {
		d.logger.Printf("fork_sync_dog: merge-base failed for %s: %v", rigName, err)
		return forkSyncFailed
	}
	mergeBase = strings.TrimSpace(mergeBase)

	// Create temporary worktree
	tmpDir, err := os.MkdirTemp("", "fork-sync-*")
	if err != nil {
		d.logger.Printf("fork_sync_dog: mkdtemp failed: %v", err)
		return forkSyncFailed
	}
	worktreePath := filepath.Join(tmpDir, "rebase")
	defer func() {
		runGitCmd(workDir, "worktree", "remove", "--force", worktreePath)
		os.RemoveAll(tmpDir)
	}()

	if _, err := runGitCmd(workDir, "worktree", "add", worktreePath, "fork/main", "--quiet"); err != nil {
		d.logger.Printf("fork_sync_dog: worktree add failed for %s: %v", rigName, err)
		return forkSyncFailed
	}

	// Rebase onto origin/main
	if _, err := runGitCmd(worktreePath, "rebase", "--onto", "origin/main", mergeBase); err != nil {
		runGitCmd(worktreePath, "rebase", "--abort")
		d.logger.Printf("fork_sync_dog: rebase conflict for %s — manual intervention required", rigName)
		d.logger.Printf("fork_sync_dog:   origin/main: %s", originMain[:12])
		d.logger.Printf("fork_sync_dog:   fork/main:   %s", forkMain[:12])
		d.logger.Printf("fork_sync_dog:   merge-base:  %s", mergeBase[:12])
		return forkSyncFailed
	}

	// Push rebased result
	if _, err := runGitCmd(worktreePath, "push", "fork", fmt.Sprintf("HEAD:main"), "--force-with-lease"); err != nil {
		d.logger.Printf("fork_sync_dog: force-push failed for %s: %v", rigName, err)
		return forkSyncFailed
	}

	d.logger.Printf("fork_sync_dog: %s fork synced (rebased local commits onto upstream)", rigName)
	return forkSyncSynced
}

// findRigWorkDir finds a git working directory for the given rig.
// Checks refinery clone first, then falls back to any worktree.
func (d *Daemon) findRigWorkDir(rigName string) string {
	rigPath := filepath.Join(d.config.TownRoot, rigName)

	// Check refinery clone (canonical main clone)
	refineryDir := filepath.Join(rigPath, "refinery", "rig")
	if isGitWorkDir(refineryDir) {
		return refineryDir
	}

	// Check for .repo.git (bare repo) with any worktree
	repoGit := filepath.Join(rigPath, ".repo.git")
	if _, err := os.Stat(repoGit); err == nil {
		// Find any worktree for this repo
		worktreesDir := filepath.Join(repoGit, "worktrees")
		entries, err := os.ReadDir(worktreesDir)
		if err == nil {
			for _, entry := range entries {
				gitdirFile := filepath.Join(worktreesDir, entry.Name(), "gitdir")
				data, err := os.ReadFile(gitdirFile)
				if err == nil {
					wtPath := strings.TrimSpace(string(data))
					// gitdir file contains path to the .git file in the worktree
					wtDir := filepath.Dir(wtPath)
					if isGitWorkDir(wtDir) {
						return wtDir
					}
				}
			}
		}
	}

	// Check polecats as fallback
	polecatsDir := filepath.Join(rigPath, "polecats")
	entries, err := os.ReadDir(polecatsDir)
	if err != nil {
		return ""
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		// Polecat worktrees have a subdirectory named after the rig's repo
		subEntries, err := os.ReadDir(filepath.Join(polecatsDir, entry.Name()))
		if err != nil {
			continue
		}
		for _, sub := range subEntries {
			candidate := filepath.Join(polecatsDir, entry.Name(), sub.Name())
			if isGitWorkDir(candidate) {
				return candidate
			}
		}
	}

	return ""
}

// isGitWorkDir checks if a directory is a valid git working directory.
func isGitWorkDir(dir string) bool {
	gitPath := filepath.Join(dir, ".git")
	_, err := os.Stat(gitPath)
	return err == nil
}
