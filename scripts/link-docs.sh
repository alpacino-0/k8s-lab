#!/usr/bin/env bash
# Makes the private working notes readable inside a linked worktree.
#
#   scripts/link-docs.sh
#
# The problem it exists for, measured rather than imagined: PLAN.md, DURUM.md,
# KARARLAR.md, GUVENLIK.md and OLCUMLER.md are excluded by .gitignore on
# purpose — they are working notes and the decision not to publish them is a
# decision, not an oversight. Excluded means they are in no commit, and a linked
# worktree is built from a commit. So every instruction of the form "read
# docs/PLAN.md, the scope boundary is there" names a file that is not in the
# directory the reader is standing in. One session worked around it by reading
# the main checkout directly and said so; the others were never asked, and
# whether they read anything at all is not knowable after the fact.
#
# ## Symlinks and not copies, and the reason is not tidiness
#
# The two answers fail differently and only one of them fails where somebody is
# looking.
#
# A copy goes stale silently. Worse — and this is the decisive half — a copy is
# writable, and these notes are never committed, so an edit made in a worktree
# against a copy is not in any ref, not in the main checkout, and not anywhere a
# merge would find it. It is simply gone the day the worktree is removed. This
# script exists because agents read these files; some of them also write them.
#
# A symlink that outlives its target fails loudly and immediately: every reader
# gets "No such file or directory" naming the exact path, which is a sentence
# that sends somebody to the right place. And a write through it lands in the
# one copy that exists.
#
# The link is absolute rather than relative because a worktree can be created
# anywhere, and a relative path encodes an assumption about where the two trees
# sit that nothing enforces.
#
# ## Where "the original" is
#
# The first entry of `git worktree list` is the main working tree — the
# non-linked one — and that is where files outside every commit live. Asked
# rather than configured, so this keeps working when the checkout moves.
set -euo pipefail

cd "$(git rev-parse --show-toplevel)"
here="$PWD"

main="$(git worktree list --porcelain | awk '/^worktree /{print substr($0, 10); exit}')"
if [[ -z "$main" ]]; then
  echo "link-docs: cannot tell which worktree is the main one" >&2
  exit 1
fi

if [[ "$main" == "$here" ]]; then
  echo "link-docs: this is the main checkout; the notes are already here"
  exit 0
fi
if [[ ! -d "$main/docs" ]]; then
  echo "link-docs: $main/docs does not exist, so there is nothing to link" >&2
  exit 1
fi

mkdir -p docs
linked=0
skipped=0
for source in "$main"/docs/*.md; do
  [[ -e "$source" ]] || continue
  name="$(basename "$source")"

  # Tracked files arrive with the commit and must not be shadowed: DEPLOY.md is
  # a runbook that differs between branches, and pointing it at another
  # checkout's copy would silently show the wrong branch's instructions.
  if git ls-files --error-unmatch "docs/$name" >/dev/null 2>&1; then
    continue
  fi

  # A real file here is somebody's work, and this script is not entitled to it.
  # Only a link it made itself is replaced, which is what makes rerunning safe.
  if [[ -e "docs/$name" && ! -L "docs/$name" ]]; then
    echo "link-docs: docs/$name is a real file here, leaving it alone" >&2
    skipped=$((skipped + 1))
    continue
  fi

  ln -sfn "$source" "docs/$name"
  linked=$((linked + 1))
done

echo "link-docs: linked $linked note(s) from $main/docs"
[[ "$skipped" -eq 0 ]] || echo "link-docs: left $skipped file(s) alone"
