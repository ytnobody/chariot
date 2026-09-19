# chariot

A small Go CLI that pipes stdin lines into [TypeSafe's Jev](https://typesafe.ai)
(a System One judgment model) and streams typed answers back out. Each input
line becomes the `state` of one judgment; the question itself (type,
instructions, criteria) is fixed for the whole run.

## Build

```
go build -o chariot .
```

## Setup

Get an API key at https://typesafe.ai, then:

```
export TYPESAFE_API_KEY=your_api_key
```

Running any subcommand without this set exits with an error explaining how
to set it.

## Usage

```
chariot <choice|score|noul> [options] <instructions> [option ...]
```

- `choice`, `score`, `noul` are System One's three judgment primitives
  (see [docs.typesafe.ai](https://docs.typesafe.ai/primitives)).
- `<instructions>` is the natural-language question, e.g. `"is this urgent?"`.
- Extra trailing words become the criteria (see below) — handy for `choice`
  and `score`, which need a defined set of options or levels.

Input is read one line per judgment, from stdin by default. Output goes to
stdout, one line per result, streamed as each answer completes (not
buffered until the whole input ends).

### Examples

```
echo "Help! My payouts have failed for 3 days." | chariot noul "does this convey urgency?"

chariot choice "which category is this?" billing shipping returns

chariot score "how angry does this sound?" calm irritated furious
```

## Options

Every option has a one-letter shorthand; long and short forms behave
identically.

| Long | Short | Default | Meaning |
| --- | --- | --- | --- |
| `-criteria` | `-c` | (none) | Criteria for the judgment. See below. |
| `-model` | `-m` | `jev-latest` | System One model to use. |
| `-concurrency` | `-p` | `8` | Max concurrent requests. |
| `-format` | `-f` | `json` | Output format: `json` (NDJSON) or `tsv`. |
| `-fifo` | `-i` | (none) | Read from a named pipe instead of stdin. |

## Criteria

`choice` and `score` need criteria; `noul` usually doesn't. There are three
ways to give them:

1. **Trailing words** on the command line (simplest):
   ```
   chariot choice "which category is this?" billing shipping returns
   ```
2. **`-criteria`/`-c`** with a shorthand string — `"key: description"` entries
   separated by commas, semicolons, or newlines (mixing plain names and
   `key: description` entries is fine):
   ```
   chariot choice -c "returns: exchanges, refunds; shipping: delays" "which category?"
   ```
3. **Raw JSON**, inline or from a file with `@`, for cases the shorthand
   can't express (nested objects, `examples`, etc.):
   ```
   chariot choice -c @criteria.json "which category?"
   ```

Trailing words and `-criteria` are mutually exclusive — use one or the other.

**The shape differs by type**, matching what the API requires:
- `choice` needs a **dictionary** (option name → description). The shorthand
  builds this; a plain name with no `:` maps to itself.
- `score` needs an **ordered list** of level descriptions, low to high. The
  shorthand builds a list in the order given; `key: description` entries
  keep only the description (the key is dropped since order — not naming —
  defines the level).

  `score` is for a single ordinal dimension (e.g. calm → furious). Don't use
  it for unrelated categories with no natural order — that's what `choice`
  is for. A single-item criteria list also isn't a real scale: with only one
  level, the answer is trivially that level at full confidence.

## Output formats

**json** (default) — one NDJSON object per line:
```json
{"line":1,"input":"...","answer":{"type":"noul","noul":0.92}}
```
`answer` is the raw System One response for that question.

**tsv** — `line`, `input`, `value`, `confidence`, tab-separated:
```
1	Help! My payouts have failed.	0.92	
```
`value` is the probability for `noul`, the picked option for `choice`, or
the weighted score for `score`. `confidence` is empty for `noul` (it has
none) and filled in for `choice`/`score`.

Low confidence on `choice`/`score` means the model's answer is spread
across options rather than concentrated on one — treat it as "uncertain,"
not as a specific value.

## Streaming input

By default, chariot reads from stdin until EOF, processing and printing
each line as it arrives (real streaming, not batched). Interactive
terminals get a one-time notice on stderr indicating that it's in input
mode; piped input does not.

For input that can come from more than one process over time, use a named
pipe with `-fifo`/`-i`:

```
mkfifo /tmp/input.fifo
chariot noul -i /tmp/input.fifo "urgent?" &

echo "some text" > /tmp/input.fifo   # any process, any time
echo "more text" > /tmp/input.fifo   # even after the pipe was closed and reopened
```

A FIFO reader normally exits when its writers close it, but chariot
reopens it and keeps waiting for the next writer — so the reader process
can run indefinitely while different processes send it work over time.
Exit with Ctrl-C.

## Development

```
go test ./...
```
