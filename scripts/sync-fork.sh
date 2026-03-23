#!/bin/bash
# =============================================================================
# SYNC-FORK: Rebase fork/main onto origin/main (upstream)
# =============================================================================
#
# Keeps a GitHub fork synchronized with its upstream repository. Detects
# the 'fork' remote, fetches both origin (upstream) and fork, then:
#   - Fast-forwards fork/main if it's simply behind upstream
#   - Rebases local-only commits onto upstream if diverged
#   - Escalates on conflict (non-zero exit)
#
# Usage:
#   sync-fork.sh [REPO_DIR]
#
# Arguments:
#   REPO_DIR  Path to the git repository (default: current directory)
#
# Environment:
#   SYNC_FORK_QUIET=1  Suppress informational output
#
# Exit codes:
#   0  Success (synced or already up-to-date)
#   1  Rebase conflict or fatal error (requires manual intervention)
#
# =============================================================================

set -euo pipefail

REPO_DIR="${1:-.}"
QUIET="${SYNC_FORK_QUIET:-}"

info() {
    [ -n "$QUIET" ] || echo "$@"
}

cd "$REPO_DIR"

# Verify this is a git repository
if ! git rev-parse --git-dir >/dev/null 2>&1; then
    echo "Error: not a git repository: $REPO_DIR" >&2
    exit 1
fi

# Check if 'fork' remote exists — no fork remote means no fork workflow
if ! git remote | grep -q '^fork$'; then
    info "No 'fork' remote found, skipping sync."
    exit 0
fi

# Fetch latest from both remotes
if ! git fetch origin main --quiet 2>/dev/null; then
    echo "Warning: failed to fetch origin main" >&2
    exit 0
fi

if ! git fetch fork main --quiet 2>/dev/null; then
    echo "Warning: failed to fetch fork main" >&2
    exit 0
fi

ORIGIN_MAIN=$(git rev-parse origin/main 2>/dev/null) || { echo "Warning: origin/main not found" >&2; exit 0; }
FORK_MAIN=$(git rev-parse fork/main 2>/dev/null) || { echo "Warning: fork/main not found" >&2; exit 0; }

# Already in sync
if [ "$ORIGIN_MAIN" = "$FORK_MAIN" ]; then
    info "Fork already up to date with upstream."
    exit 0
fi

# Case 1: Fork is behind upstream — fast-forward push
if git merge-base --is-ancestor "$FORK_MAIN" "$ORIGIN_MAIN"; then
    info "Fork is behind upstream, pushing fast-forward..."
    if git push fork origin/main:main 2>/dev/null; then
        info "Fork synced (fast-forward)."
    else
        echo "Warning: failed to push fast-forward to fork" >&2
    fi
    exit 0
fi

# Case 2: Fork is ahead of upstream — local commits exist, upstream will catch up
if git merge-base --is-ancestor "$ORIGIN_MAIN" "$FORK_MAIN"; then
    info "Fork is ahead of upstream (local commits pending merge). Nothing to do."
    exit 0
fi

# Case 3: Diverged — need rebase via temporary worktree
info "Fork and upstream have diverged. Attempting rebase..."

TMPDIR=$(mktemp -d)
WORKTREE="$TMPDIR/sync-fork-rebase"
cleanup() {
    git worktree remove --force "$WORKTREE" 2>/dev/null || true
    rm -rf "$TMPDIR"
}
trap cleanup EXIT

# Create temporary worktree at fork/main
git worktree add "$WORKTREE" fork/main --quiet 2>/dev/null

# Perform rebase in the worktree
MERGE_BASE=$(git merge-base "$FORK_MAIN" "$ORIGIN_MAIN")
if ! git -C "$WORKTREE" rebase --onto origin/main "$MERGE_BASE"; then
    git -C "$WORKTREE" rebase --abort 2>/dev/null || true
    echo "ERROR: Rebase conflict during fork sync. Manual intervention required." >&2
    echo "  Upstream (origin/main): $(git rev-parse --short origin/main)" >&2
    echo "  Fork (fork/main):       $(git rev-parse --short fork/main)" >&2
    echo "  Merge base:             $(git rev-parse --short "$MERGE_BASE")" >&2
    exit 1
fi

# Push rebased result
if git -C "$WORKTREE" push fork HEAD:main --force-with-lease; then
    info "Fork synced (rebased local commits onto upstream)."
else
    echo "ERROR: Failed to push rebased fork/main (--force-with-lease rejected)." >&2
    echo "  Another push may have occurred concurrently. Retry later." >&2
    exit 1
fi
