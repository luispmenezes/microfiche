# microfiche

Cut Claude Code token costs by serving large read-only files as rendered
images instead of text. Named after the original optical-compression
medium.

An [MCP](https://modelcontextprotocol.io) server exposing one tool,
`microfiche(file_path, page)`. When Claude needs a big reference file —
docs, logs, transcripts, source it won't edit — the tool returns a dense
PNG render of the content instead of text. Claude's image tokens are
priced by pixels (~750 px²/token), so packed text costs 2–3x fewer input
tokens than the same characters as text tokens.

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
- **Auto-bail**: files under ~5k estimated tokens, or that wouldn't
  compress at least 1.25x, return plain text — below that break-even the
  fixed harness overhead eats the saving.
- Renders are cached on (path, mtime, page); per-call telemetry is
  appended to `~/.microfiche/log.jsonl`.

## Benchmarks

Measured through headless Claude Code (`claude -p`), baseline `Read` vs
microfiche, same file and question, answers verified equivalent.

Savings and latency by file size (Claude Fable 5, fable profile, mean of
2 runs per size):

| file size | input tokens | net cost | wall time |
|---|---|---|---|
| ≤ ~18 KB | auto-bails to plain text | ±0% | ±0% |
| 20 KB | −41% | **−31%** | +9% |
| 40 KB | −56% | **−49%** | **−15% (faster)** |
| 60 KB | −66% | **−58%** | **−16% (faster)** |
| 120 KB (2 pages) | +16% | +18% (loses) | +40% |

The sweet spot is single-page files (~20–60 KB): cost falls with size,
and from ~40 KB microfiche is also *faster* than Read — vision-encoding
one image beats prefilling tens of thousands of text tokens. Small files
lose (prompt caching makes cached re-reads nearly free, and the tool
round-trip costs more than it saves — hence the auto-bail); multi-page
files (> ~60 KB) currently lose too, since each page is a separate
round-trip.

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

Grab a prebuilt binary from
[Releases](https://github.com/luispmenezes/microfiche/releases)
(macOS arm64/amd64, Linux amd64/arm64, Windows amd64), or:

```sh
go install github.com/luispmenezes/microfiche@latest
```

Single static binary, no runtime deps; needs a monospace system font
(Menlo/Monaco on macOS, DejaVu Sans Mono on Linux, Consolas on Windows).

### Claude Code

```sh
claude mcp add --scope user microfiche -- /path/to/microfiche                 # Fable 5
claude mcp add --scope user microfiche -- /path/to/microfiche -profile opus  # Opus 4.x
```

### Codex CLI

Add to `~/.codex/config.toml`:

```toml
[mcp_servers.microfiche]
command = "/path/to/microfiche"
args = ["-profile", "opus"]
```

### Cursor

Add to `~/.cursor/mcp.json` (or `.cursor/mcp.json` per project):

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
