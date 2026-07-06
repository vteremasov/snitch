# snitch

A small Go CLI for watching and controlling multiple Claude Code sessions
from one place. It wraps `claude` under a pseudo-terminal, exposes a
bubbletea dashboard that lists every running wrapper, and lets you toggle a
per-session "auto-yes" mode that auto-approves permission prompts.

It exists because running many parallel `claude` sessions means flipping
between terminals to accept permission prompts. With snitch, one dashboard
shows what each session is doing and what it's blocked on, and a single
keystroke approves the prompt — or auto-yes can do it for you.

## What it does

- **`snitch run [-- claude-args…]`** — runs `claude` under a pty owned by
  snitch. Passthrough is byte-for-byte: paste, image paste, alt-screen,
  hyperlinks, mouse, resize, signals, true color all work the same as
  running `claude` directly.
- **`snitch dash`** — a full-screen TUI. Lists every running `snitch run`
  wrapper, what each session is currently doing, and any pending permission
  prompt. Toggle auto-yes per session, approve a prompt manually, or
  bulk-toggle all sessions.
- **`snitch ls`** — one-shot snapshot of active wrappers, scriptable.

## Architecture

Two-process model:

```
┌─ Terminal A ──────────┐  ┌─ Terminal B ──────────┐  ┌─ Terminal C ──────────┐
│ $ snitch run          │  │ $ snitch run          │  │ $ snitch dash         │
│ <claude TUI here>     │  │ <claude TUI here>     │  │ <bubbletea dashboard> │
└───────────────────────┘  └───────────────────────┘  └───────────────────────┘
       │ owns pty                  │ owns pty                  │
       └──── unix socket ──────────┴────── unix socket ────────┘
              ~/.snitch/sock/<pid>.sock
```

Each `snitch run` is a stand-alone wrapper that:

1. Spawns `claude` under a fresh pty (passthrough to the parent terminal).
2. Watches `~/.claude/sessions/<claudePID>.json` for the session id and
   tails the matching transcript at
   `~/.claude/projects/<encoded-cwd>/<sessionId>.jsonl`.
3. Registers itself at `~/.snitch/sessions/<wrapperPID>.json` and listens
   on `~/.snitch/sock/<wrapperPID>.sock`.
4. When a `tool_use` arrives without a matching `tool_result` and auto-yes
   is on, writes `\r` to the pty master to approve the prompt.

`snitch dash` discovers wrappers by scanning `~/.snitch/sessions/`,
connects to each socket, subscribes for state pushes, and fans out
toggle/approve commands.

### Repo layout

```
snitch/
  main.go                 # subcommand dispatch: run | dash | ls
  internal/
    state/                # shared types, control-socket protocol, paths
    transcript/           # jsonl tail + event classification
    wrapper/              # pty, signals, registration, control socket, auto-yes
    discover/             # scan ~/.snitch/sessions/, prune dead pids
    dash/                 # bubbletea Model/Update/View, control-socket client
```

### Files snitch creates

- `~/.snitch/sessions/<wrapperPID>.json` — current state of one wrapper.
- `~/.snitch/sock/<wrapperPID>.sock` — wrapper control socket.
- `~/.snitch/log/<wrapperPID>.log` — wrapper debug log (tail this when
  iterating on the auto-yes heuristic).

## Setup

Requires Go 1.22+ and the `claude` binary on `PATH`.

```sh
git clone <this repo>
cd snitch
go build -o ./snitch .
ln -sf "$PWD/snitch" ~/.local/bin/snitch   # ~/.local/bin must be on PATH
```

Drop a shell alias so every `claude` invocation is wrapped automatically:

```sh
alias claude='snitch run --'
```

After that, `claude` and `claude --resume` both go through snitch. Open a
second terminal and run `snitch dash` to see all live wrappers.

### OS notifications

The wrapper fires a desktop notification in two cases:

- **Permission needed** — a `tool_use` is pending, auto-yes is off for this
  session, and 300ms have passed since the prompt appeared. Body shows the
  tool and a short preview of its input.
- **Claude is waiting for input** — claude's status transitions to
  `waiting` with no pending permission prompt (i.e. claude finished its
  turn). Throttled to one notification per 30 seconds per wrapper so a
  flicker between states doesn't spam.

**macOS Delivery:**
- **With `terminal-notifier` (Recommended)** — If you install `terminal-notifier` (via `brew install terminal-notifier`), snitch will use it to deliver interactive notifications that automatically bring the target terminal (including JetBrains embedded terminals and Apple Terminal.app) to focus when clicked. It dynamically inspects the process hierarchy to find the correct active IDE or terminal app bundle identifier to activate.
- **Native OSC 9** — If `terminal-notifier` is not present, snitch writes the **iTerm2 OSC 9 escape sequence** to its controlling terminal. Modern terminals (Ghostty, iTerm2, WezTerm, VS Code) translate this into a native notification, and clicking it focuses the terminal window/tab.
- **AppleScript Fallback** — For other terminals, it falls back to macOS `osascript` to trigger a system banner.

**Linux Delivery:**
OSC 9 to `/dev/tty` plus `notify-send` if installed.

To turn notifications off entirely:

```sh
SNITCH_NOTIFY=0 snitch run
```

### Dashboard keys

- `↑/↓` or `j/k` — navigate
- `space` — toggle auto-yes for the selected session
- `enter` — approve the selected session's pending prompt right now
- `A` / `N` — auto-yes ON / off for all wrappers
- `p` — toggle filter to show only sessions with a pending prompt
- `w` or `c` — toggle keep-awake (uses macOS `caffeinate` to prevent system sleep while at least one wrapper is busy)
- `q` or `ctrl+c` — quit

## Tested terminals

- **Ghostty** (primary)
- **JetBrains IDE embedded terminal**

Other terminals should work too — the pty path is byte-for-byte
transparent and depends only on POSIX behavior. macOS is the primary
target; Linux should work but is untested.

## Limitations and known issues

- The pending-prompt heuristic (`tool_use` with no matching `tool_result`
  for ≥300ms) is best-effort. A slow but already-approved tool can
  briefly look like a pending prompt. The 800ms debounce keeps that from
  cascading, but if it's noisy in your workflow, tighten the rule by
  observing what events your transcript actually emits — the wrapper
  logs to `~/.snitch/log/<pid>.log` for exactly this.
- Claude Code's transcript schema (`.jsonl` under `~/.claude/projects/`)
  isn't a stable public contract. snitch decodes permissively
  (`map[string]any` with field-by-field type assertions) so unfamiliar
  shapes degrade rather than crash, but expect occasional fix-up after
  Claude Code updates.
- Auto-yes injects a literal `\r`. If Claude Code ever changes the
  default-highlighted button on a permission prompt away from "Yes",
  this becomes a no-op (or worse, picks the wrong choice). The
  injection itself can be replaced or augmented with a different byte
  sequence in `internal/wrapper/run.go`.
- The wrapper takes over the parent terminal as long as `claude` runs.
  If you `kill -9` the wrapper, claude will see its pty closed and exit
  too — that's intentional. A clean `kill` (default SIGTERM) propagates
  to claude.

## Contributing

PRs and patches welcome. A few rules of thumb that will save review
back-and-forth:

### Don't break passthrough

The pty data path is sacred. `internal/wrapper/pty.go` must:

- never line-buffer, scan, or transform stdin/stdout bytes,
- only ever serialize writes to the pty master via the existing
  `writeMu` (auto-yes injection lives behind it),
- only observe the input byte stream for paste-window tracking — never
  modify it.

If you find yourself reaching for `bufio.Scanner` or `strings.Replace`
in the I/O path, stop.

### Keep the transcript decoder lenient

`internal/transcript/events.go` decodes into `map[string]any` and
type-asserts each field. New event shapes from Claude Code should
degrade to `KindOther` rather than crash. When adding a new
classification, log unfamiliar shapes via the wrapper's debug log so
they show up in `~/.snitch/log/<pid>.log` during real use — that's
faster than guessing the schema.

### Wire new dashboard actions through the control socket

The dashboard never reads/writes pty state directly — it's always:

```
dash key → Client.<Op>() → unix socket → wrapper handler → broadcast
```

If you want a new action, add it as a new `Op` in
`internal/state/protocol.go`, handle it in
`internal/wrapper/control.go`, and add the key in
`internal/dash/update.go`. Keep optimistic updates per-session, never
across all sessions.

### Style

- `go fmt`, `go vet`, `go build` should all pass cleanly. Run them
  before pushing.
- No comments that just restate what the code does. Comments are for
  the *why*: a non-obvious invariant, a hidden constraint, a known
  surprise. Most of the code shouldn't need any.
- Prefer editing existing files over creating new ones. Internal
  packages are small and focused on purpose; resist the urge to
  abstract until there are 3 concrete cases.

### Local development

```sh
go build ./... && go vet ./...     # sanity
go build -o ./snitch .             # rebuild the symlinked binary
```

There's no test suite yet — the parts that matter (pty passthrough,
sessionId rotation, transcript classification) are easier to validate
manually against a real `claude` session than to mock. If you add
something that's clean to unit-test (parsers, path helpers,
classifier), please add tests for it.

When iterating on the auto-yes heuristic, leave a real session running
and `tail -f ~/.snitch/log/<wrapperPID>.log` while you trigger
permissions. The log includes every `tool_use` classification, every
auto-yes fire decision, and every manual `approve_now` from the
dashboard — that trace is usually enough to find the regression.
