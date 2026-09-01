# templates

The one-click catalogue. These compose files are copied **verbatim** from
[coollabsio/coolify](https://github.com/coollabsio/coolify) (`templates/compose/`),
which is licensed **Apache-2.0**. The licence text is beside them in
[LICENSE.apache-2.0](LICENSE.apache-2.0); this repository as a whole stays
AGPL-3.0, with which Apache-2.0 is compatible.

Apache-2.0 permits the copy and asks for the attribution above and the licence
alongside. Both are here, and the source is traceable to one commit rather than
to "upstream":

| | |
|---|---|
| Source | `https://github.com/coollabsio/coolify` |
| Path | `templates/compose/` |
| Commit | `2018e7f32910de6ee059fd153ae1172fc3300594` |
| Dated | 2026-09-01 |
| Files | 371 (368 `.yaml`, 3 `.yml`) |
| Bytes | 940,292 |

Refreshed by repeating exactly what produced them, which is why the commit is
written down rather than "latest":

```sh
git clone --depth 1 --filter=blob:none --sparse https://github.com/coollabsio/coolify.git
cd coolify && git sparse-checkout set templates/compose
cp templates/compose/*.yaml templates/compose/*.yml <this directory>/
```

**Do not edit them.** A template edited to suit this platform stops being
evidence that this platform handles the corpus, and the next refresh would
silently undo the edit. Everything this platform cannot express is reported by
the converter as a note and by the install endpoint as a refusal — that is where
a disagreement belongs, not here.

## Why all 371 and not the ones that install

All of them, and the alternative was measured rather than assumed. Counted
through `whyRefused` on 2026-09-01, against this corpus: **341 entries are
offered** (30 are skipped — 28 carry upstream's own `# ignore: true` and 2 are
not valid YAML), and **59 of the 341 install as they stand.**

Shipping only those 59 would be selecting against two limits that are neither
the template's fault nor permanent:

- **161** are refused first because an image carries no tag or uses `:latest`,
  which lifts the moment something resolves a tag to a digest at install time.
- **26** are refused first because the template becomes more than one object,
  which lifts when the write path can commit more than one file per placement.

Both are being written now. A subset chosen against them would be the wrong
subset this week, and the criterion would have to be re-argued at every refresh.

The endpoint already tells the truth per entry: `whyRefused` lists every reason
an install will not happen, and `POST .../from-catalog` with `dryRun` shows them
before anything is written. An entry that is listed and refuses is the product
explaining itself. An entry that was never vendored is a question nobody can
ask.

The cost of the choice is 940 KB in the repository and in the control plane's
image, which is smaller than the Go binary beside it.
