# microfiche

Cut Claude Code token costs by serving large read-only files as rendered
images instead of text.

The rationale: not everything in a model's context deserves text-token
prices. What an agent reads splits into a working set (files it edits,
values it must copy byte-for-byte — this must stay text) and bulk
reference material it only needs to *understand* — docs, logs,
transcripts, source it won't touch. Claude prices image tokens by pixels
(~750 px²/token), so that second kind can travel as a dense PNG render
at 2–3x fewer input tokens, while the exact-value parts ride along as
plain text.

microfiche is an [MCP](https://modelcontextprotocol.io) server exposing
one tool, `microfiche(file_path, page)`, that does exactly that split.

## How it works

- **Density is calibrated per model.** Text renders at ~48 px²/char
  (`-profile fable`) or ~72 px²/char (`-profile opus`). Past the
  legibility cliff (~35–40 px²/char) models don't fail loudly — they
  silently confabulate exact strings. Both profiles stay above it.
- **A verbatim factsheet rides along as text**: hashes, versions,
  constants, URLs, and numbers are regex-extracted and repeated in plain
  text, because exact values must never be trusted to pixels. Lines
  containing them are also rendered twice, in red.
- **Single-pass instruction**: the tool result tells the model not to
  crop/zoom the image (agentic zooming burns more output tokens than the
  input saving), and to fall back to the regular `Read` tool for anything
  illegible or byte-exact.
- **Multi-page in one response**: big files come back as up to 4 dense
  page images in a single tool result (within a ~25k image-token
  budget), so a 120 KB file is one round-trip.
- **Knows when not to fire**: files under ~5k estimated tokens (or that
  wouldn't compress ≥1.25x) return plain text; files over ~200 KB are
  refused with Grep-then-`line_start`/`line_end` guidance — both
  boundaries are where the benchmarks say imaging stops paying.
- Renders are cached on (path, mtime, page); every call is logged to
  `~/.microfiche/log.jsonl` — run `microfiche -stats` to see imaged vs
  bailed vs skipped calls and estimated tokens/dollars saved.

## Benchmarks

Measured through headless Claude Code (`claude -p`), baseline `Read` vs
microfiche, same file and question, answers verified equivalent.

Savings and latency by file size (Claude Fable 5, fable profile, mean of
2 runs per size):

| file size | input tokens | net cost | wall time |
|---|---|---|---|
| ≤ ~18 KB | auto-bails to plain text | ±0% | ±0% |
| 20 KB | −43% | **−36%** | **−10% (faster)** |
| 40 KB | −55% | **−49%** | **−19% (faster)** |
| 60 KB | −63% | **−55%** | **−28% (faster)** |
| 120 KB | −37% | **−33%** | −4% |
| ≥ ~200 KB | refused — Grep + line range instead | — | — |

Cheaper *and* faster across the whole 20–120 KB range: vision-encoding a
couple of dense images beats prefilling tens of thousands of text
tokens. The edges are handled by design: small files bail to plain text
(prompt caching already makes cached re-reads nearly free), and very
large files are refused with guidance to Grep for the region and re-call
with `line_start`/`line_end` — at that size full-file ingestion merely
breaks even and targeted reading wins.

Model profiles: Opus 4.6 with `-profile opus` on a 58 KB doc measured
−29% cost at ±0% time. Exact-string recall stayed 100% in every cell
*with the matching profile*; the dense fable profile on Opus drops exact
recall to ~33% (silent misreads). Match the profile to your model.

Reproduce on your own files and model:

```sh
./microfiche -bench path/to/big_file.txt -model claude-fable-5 -n 2
./microfiche -bench path/to/big_file.txt -model claude-opus-4-6 -profile opus
```

## Setup

One command — downloads the right binary and registers it with Claude
Code, Codex, and Cursor (whichever are installed):

```sh
curl -fsSL https://raw.githubusercontent.com/luispmenezes/microfiche/main/install.sh | sh
```

Running an Opus model? `MICROFICHE_PROFILE=opus curl -fsSL ... | sh`.
macOS and Linux; on Windows grab the `.exe` from
[Releases](https://github.com/luispmenezes/microfiche/releases) and
register manually (below).

<details>
<summary>Manual setup</summary>

Download from [Releases](https://github.com/luispmenezes/microfiche/releases)
or `go install github.com/luispmenezes/microfiche@latest`. Single static
binary; needs a monospace system font (Menlo/Monaco on macOS, DejaVu
Sans Mono on Linux, Consolas on Windows).

**Claude Code**

```sh
claude mcp add --scope user microfiche -- /path/to/microfiche [-profile opus]
```

**Codex CLI** — `~/.codex/config.toml`:

```toml
[mcp_servers.microfiche]
command = "/path/to/microfiche"
args = ["-profile", "opus"]
```

**Cursor** — `~/.cursor/mcp.json` (or `.cursor/mcp.json` per project):

```json
{
  "mcpServers": {
    "microfiche": {
      "command": "/path/to/microfiche",
      "args": ["-profile", "opus"]
    }
  }
}
```

</details>

Pick the profile for the model you run: `fable` for Claude Fable 5,
`opus` for Claude Opus 4.x. On non-Claude models (GPT, Gemini) the
density calibration and token math are untested — start with `-profile
opus` and run `-bench` before trusting it.

**Optional — raise the trigger rate.** A hint in your `CLAUDE.md` /
`AGENTS.md` / Cursor rules makes the model reach for the tool
consistently:

```markdown
For reference files over ~20KB that you will NOT edit (logs, docs,
transcripts, data dumps), use the microfiche tool instead of Read.
Keep using Read for anything you will edit or need byte-exact.
```

To preview what the model sees:
`./microfiche -render some_file.txt > preview.png`.

## When to use it

Doc-, log-, and transcript-heavy work over files in the 20–60 KB sweet
spot, where it is both cheaper and (above ~40 KB) faster than a plain
Read. On smaller files it bails to text by design; don't point it at
files you're editing or anything needing byte-exact recall.
