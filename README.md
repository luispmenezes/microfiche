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
microfiche, same file and question, answers verified equivalent:

| model / profile | file | input tokens | net cost | wall time |
|---|---|---|---|---|
| Fable 5 / fable | 58 KB doc | −57 to −62% | **−37 to −53%** | +12 to +52% |
| Opus 4.6 / opus | 58 KB doc | −37% | **−29%** | ±0% |
| any / any | ~18 KB doc | ~flat | +10 to +60% (loses) | +50 to 175% |

Small files lose: prompt caching already makes cached re-reads nearly
free, and the extra tool round-trip plus image reasoning cost more than
the saving — hence the auto-bail. Exact-string recall stayed 100% in all
benchmark cells *with the matching profile*; running the dense fable
profile on Opus drops exact recall to ~33% (silent misreads). Match the
profile to your model.

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

Latency-tolerant sessions over big files: batch/background agents,
overnight runs, doc- and log-heavy work. It is ~1.1–1.5x slower per read
and never faster — the win is cost, not speed. Don't point it at files
you're editing; it refuses small files by design.
