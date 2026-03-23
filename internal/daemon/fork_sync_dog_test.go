package daemon

import (
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestForkSyncDogInterval(t *testing.T) {
	t.Run("default", func(t *testing.T) {
		got := forkSyncDogInterval(nil)
		if got != defaultForkSyncDogInterval {
			t.Errorf("forkSyncDogInterval(nil) = %v, want %v", got, defaultForkSyncDogInterval)
		}
	})

	t.Run("configured", func(t *testing.T) {
		config := &DaemonPatrolConfig{
			Patrols: &PatrolsConfig{
				ForkSyncDog: &ForkSyncDogConfig{
					Enabled:     true,
					IntervalStr: "30m",
				},
			},
		}
		got := forkSyncDogInterval(config)
		want := 30 * time.Minute
		if got != want {
			t.Errorf("forkSyncDogInterval = %v, want %v", got, want)
		}
	})

	t.Run("invalid_interval_falls_back", func(t *testing.T) {
		config := &DaemonPatrolConfig{
			Patrols: &PatrolsConfig{
				ForkSyncDog: &ForkSyncDogConfig{
					Enabled:     true,
					IntervalStr: "bad",
				},
			},
		}
		got := forkSyncDogInterval(config)
		if got != defaultForkSyncDogInterval {
			t.Errorf("forkSyncDogInterval = %v, want %v (default)", got, defaultForkSyncDogInterval)
		}
	})
}

func TestIsPatrolEnabled_ForkSyncDog(t *testing.T) {
	t.Run("nil_config", func(t *testing.T) {
		if IsPatrolEnabled(nil, "fork_sync_dog") {
			t.Error("expected disabled for nil config")
		}
	})

	t.Run("disabled", func(t *testing.T) {
		config := &DaemonPatrolConfig{
			Patrols: &PatrolsConfig{
				ForkSyncDog: &ForkSyncDogConfig{Enabled: false},
			},
		}
		if IsPatrolEnabled(config, "fork_sync_dog") {
			t.Error("expected disabled")
		}
	})

	t.Run("enabled", func(t *testing.T) {
		config := &DaemonPatrolConfig{
			Patrols: &PatrolsConfig{
				ForkSyncDog: &ForkSyncDogConfig{Enabled: true},
			},
		}
		if !IsPatrolEnabled(config, "fork_sync_dog") {
			t.Error("expected enabled")
		}
	})
}

func TestIsGitWorkDir(t *testing.T) {
	tmp := t.TempDir()

	t.Run("not_git", func(t *testing.T) {
		if isGitWorkDir(tmp) {
			t.Error("expected false for non-git dir")
		}
	})

	t.Run("with_git_dir", func(t *testing.T) {
		dir := filepath.Join(tmp, "repo")
		os.MkdirAll(filepath.Join(dir, ".git"), 0755)
		if !isGitWorkDir(dir) {
			t.Error("expected true for dir with .git")
		}
	})

	t.Run("with_git_file", func(t *testing.T) {
		dir := filepath.Join(tmp, "worktree")
		os.MkdirAll(dir, 0755)
		os.WriteFile(filepath.Join(dir, ".git"), []byte("gitdir: /some/path"), 0644)
		if !isGitWorkDir(dir) {
			t.Error("expected true for dir with .git file (worktree)")
		}
	})
}

func TestSyncForkForRig_NoForkRemote(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}

	// Create a minimal rig structure with a git repo that has no fork remote
	tmp := t.TempDir()
	rigName := "test-rig"
	repoDir := filepath.Join(tmp, rigName, "refinery", "rig")
	os.MkdirAll(repoDir, 0755)

	// Init git repo
	runTestGit(t, repoDir, "init", "--initial-branch", "main")
	runTestGit(t, repoDir, "commit", "--allow-empty", "-m", "initial")

	d := &Daemon{
		config: &Config{TownRoot: tmp},
	}

	result := d.syncForkForRig(rigName)
	if result != forkSyncSkipped {
		t.Errorf("expected forkSyncSkipped for repo without fork remote, got %v", result)
	}
}

func TestFindRigWorkDir(t *testing.T) {
	tmp := t.TempDir()
	rigName := "test-rig"

	d := &Daemon{
		config: &Config{TownRoot: tmp},
	}

	t.Run("no_workdir", func(t *testing.T) {
		if dir := d.findRigWorkDir(rigName); dir != "" {
			t.Errorf("expected empty, got %q", dir)
		}
	})

	t.Run("refinery_clone", func(t *testing.T) {
		repoDir := filepath.Join(tmp, rigName, "refinery", "rig")
		os.MkdirAll(filepath.Join(repoDir, ".git"), 0755)
		defer os.RemoveAll(filepath.Join(tmp, rigName))

		dir := d.findRigWorkDir(rigName)
		if dir != repoDir {
			t.Errorf("expected %q, got %q", repoDir, dir)
		}
	})
}

func TestSyncForkForRig_AlreadySynced(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}

	tmp := t.TempDir()

	// Create "upstream" bare repo
	upstream := filepath.Join(tmp, "upstream.git")
	runTestGit(t, tmp, "init", "--bare", upstream)

	// Create "fork" bare repo (clone of upstream)
	fork := filepath.Join(tmp, "fork.git")
	runTestGit(t, tmp, "clone", "--bare", upstream, fork)

	// Create rig with refinery clone
	rigName := "test-rig"
	repoDir := filepath.Join(tmp, rigName, "refinery", "rig")
	os.MkdirAll(filepath.Dir(repoDir), 0755)

	runTestGit(t, tmp, "clone", upstream, repoDir)
	runTestGit(t, repoDir, "remote", "add", "fork", fork)
	// Push initial content to fork
	runTestGit(t, repoDir, "push", "fork", "main")

	d := &Daemon{
		config:       &Config{TownRoot: tmp},
		patrolConfig: &DaemonPatrolConfig{Patrols: &PatrolsConfig{ForkSyncDog: &ForkSyncDogConfig{Enabled: true}}},
	}
	d.logger = log.New(os.Stderr, "[fork_sync_test] ", 0)

	result := d.syncForkForRig(rigName)
	if result != forkSyncSkipped {
		t.Errorf("expected forkSyncSkipped when already synced, got %v", result)
	}
}

func TestSyncForkForRig_FastForward(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}

	tmp := t.TempDir()

	// Create "upstream" bare repo with initial commit
	upstream := filepath.Join(tmp, "upstream.git")
	upstreamWork := filepath.Join(tmp, "upstream-work")
	os.MkdirAll(upstreamWork, 0755)
	runTestGit(t, upstreamWork, "init", "--initial-branch", "main")
	runTestGit(t, upstreamWork, "commit", "--allow-empty", "-m", "initial")
	runTestGit(t, tmp, "init", "--bare", upstream)
	runTestGit(t, upstreamWork, "remote", "add", "origin", upstream)
	runTestGit(t, upstreamWork, "push", "origin", "main")

	// Create "fork" bare repo (clone of upstream at initial state)
	fork := filepath.Join(tmp, "fork.git")
	runTestGit(t, tmp, "clone", "--bare", upstream, fork)

	// Add a new commit to upstream
	runTestGit(t, upstreamWork, "commit", "--allow-empty", "-m", "upstream update")
	runTestGit(t, upstreamWork, "push", "origin", "main")

	// Create rig with refinery clone
	rigName := "test-rig"
	repoDir := filepath.Join(tmp, rigName, "refinery", "rig")
	os.MkdirAll(filepath.Dir(repoDir), 0755)
	runTestGit(t, tmp, "clone", upstream, repoDir)
	runTestGit(t, repoDir, "remote", "add", "fork", fork)
	// Push initial state to fork (before the upstream update)
	// fork is already at initial state from the clone above

	d := &Daemon{
		config:       &Config{TownRoot: tmp},
		patrolConfig: &DaemonPatrolConfig{Patrols: &PatrolsConfig{ForkSyncDog: &ForkSyncDogConfig{Enabled: true}}},
	}
	d.logger = log.New(os.Stderr, "[fork_sync_test] ", 0)

	result := d.syncForkForRig(rigName)
	if result != forkSyncSynced {
		t.Errorf("expected forkSyncSynced for fast-forward case, got %v", result)
	}

	// Verify fork/main now matches origin/main
	originMain, _ := runGitCmd(repoDir, "rev-parse", "origin/main")
	forkMain, _ := runGitCmd(repoDir, "rev-parse", "fork/main")
	// Re-fetch to see updated state
	runGitCmd(repoDir, "fetch", "fork", "main")
	forkMain, _ = runGitCmd(repoDir, "rev-parse", "fork/main")
	if strings.TrimSpace(originMain) != strings.TrimSpace(forkMain) {
		t.Errorf("fork/main (%s) != origin/main (%s) after sync",
			strings.TrimSpace(forkMain), strings.TrimSpace(originMain))
	}
}

// runTestGit runs a git command in the given directory for test setup.
func runTestGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=Test",
		"GIT_AUTHOR_EMAIL=test@test.com",
		"GIT_COMMITTER_NAME=Test",
		"GIT_COMMITTER_EMAIL=test@test.com",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v failed: %v\n%s", args, err, out)
	}
}
