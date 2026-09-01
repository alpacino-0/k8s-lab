# The working notes are not in this checkout

`docs/` holds two kinds of file. The runbooks — [DEPLOY.md](DEPLOY.md),
[CONTROL-PLANE.md](CONTROL-PLANE.md), [BACKUP-SURVEY.md](BACKUP-SURVEY.md) —
are written for other people and are committed. The rest are working notes:
the plan and its scope boundary, the decisions that are closed and why, what
works today and what is owed, the security posture, and the measurements
everything else cites. They are excluded by `.gitignore` on purpose.

That exclusion has a consequence worth stating here rather than leaving to be
discovered: a note in no commit is a note in no linked worktree. If you are
reading this in a worktree created with `git worktree add`, the five files named
above are missing from the directory you are standing in, and an instruction
that says "the scope boundary is in `docs/PLAN.md`" is naming a path that does
not exist for you.

One command fixes it, from anywhere in the worktree:

```bash
scripts/link-docs.sh
```

It symlinks each note from the main checkout — the first entry of
`git worktree list` — into `docs/`. Links rather than copies, because these
files are edited as well as read and a copy of a file that is in no commit is an
edit nothing will ever merge back. `scripts/link-docs.sh` explains the rest of
the reasoning and is safe to run twice.

If the notes are missing after that, the main checkout has moved and the links
say so by name.
