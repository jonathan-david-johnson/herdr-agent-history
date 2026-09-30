# Herdr Agent History

**A “vibe slop” approach to getting the Herdr navigation I wanted.**

This is an AI-assisted, test-backed workaround—not an official Herdr feature or a polished navigation framework. It was built by describing a workflow to a coding agent, iterating on the behavior, and testing the resulting code. Expect rough edges. The tests document the intended behavior; they do not prove that every real-world timing or input problem is solved.

The goal is simple: **go back through agents I've visited, go forward again, and pick up newly completed work before repeating the circle.**

> **Experimental scope:** Linux, one local Herdr session, and one attached Herdr client. Herdr 0.9.1 is the minimum; 0.9.3 or newer is recommended. The background tracker requires a systemd user service.

## Why this exists

Cycling through every agent and cycling through completed agents solve different problems:

- Sidebar navigation follows the list, regardless of whether an agent needs attention.
- Completion navigation skips busy agents, but viewing a completion clears its dot. “Previous” therefore doesn't necessarily take you back to where you just were.

I wanted something closer to back/forward navigation, with a completion queue attached to the forward boundary.

This plugin leaves the sidebar alone. It maintains its own unique, circular history of visited agent terminals.

## Keyboard behavior

These are the suggested bindings, not shortcuts installed automatically by the plugin:

| Shortcut | Behavior | Owner |
| --- | --- | --- |
| **Alt+C** | Previous agent in the sidebar, regardless of state | Native Herdr |
| **Alt+T** | Next agent in the sidebar, regardless of state | Native Herdr |
| **Alt+H** | Previous visited agent; wrap backward at the beginning | This plugin |
| **Alt+N** | Next visited agent; at the end, prefer an unseen completion before wrapping | This plugin |

### Back, then forward

```text
mcps → herdr
         ↑ current
```

Alt+H goes to `mcps`. Alt+N returns to `herdr`. History traversal moves a cursor; it does not record another ordinary visit or reorder the list.

### Back, then somewhere new

Go back to `mcps`, then select `ubuntu` with the mouse, a picker, or ordinary sidebar navigation:

```text
mcps → ubuntu → herdr
         ↑ current
```

The new visit is inserted after the current position. The existing forward tail is preserved, unlike browser history. Alt+H returns to `mcps`; Alt+N goes to `herdr`.

Each terminal appears once. An ordinary revisit moves its existing entry rather than creating a duplicate.

### Completed work at the forward boundary

At the end of history, if `Costco` has an unseen completion, Alt+N visits it instead of wrapping:

```text
mcps → ubuntu → herdr → Costco
                           ↑ current
```

Alt+H can now return to `herdr`, and Alt+N can return to `Costco`, even after viewing Costco cleared its completion dot.

A completion never interrupts an existing forward path. At the boundary, the plugin searches after the current agent in Herdr's **server agent-list order**, wrapping that search if necessary. If no eligible completion exists, it wraps the visited history instead.

A completion already in history moves to the end. Closed or no-longer-agent terminals are pruned. With one history entry and no other completion, navigation is a no-op.

## Install

### Requirements

- Linux with a working systemd user manager.
- Herdr 0.9.1 or newer; prefer 0.9.3 or newer.
- Go 1.22 or newer to build. No third-party Go dependencies or runtime language dependency.
- One local default Herdr session and one attached client.

Clone this repository into **`~/code/herdr-agent-history`**:

```sh
mkdir -p ~/code
git clone https://github.com/jonathan-david-johnson/herdr-agent-history.git \
  ~/code/herdr-agent-history
```

The supplied service expects that checkout path; edit its `WorkingDirectory`, executable condition, and `ExecStart` if you choose another location.

Build, test, and link the plugin:

```sh
cd ~/code/herdr-agent-history
go test ./...
go build -trimpath -o herdr-agent-history ./cmd/herdr-agent-history
herdr plugin link .
```

`herdr plugin link` does not run the manifest's build command. Build first.

Install and start the tracker:

```sh
mkdir -p ~/.config/systemd/user
cp systemd/herdr-agent-history.service ~/.config/systemd/user/
systemctl --user daemon-reload
systemctl --user enable --now herdr-agent-history.service
```

The tracker starts when your user service manager starts and reconnects if Herdr is temporarily unavailable. It is supervised by systemd, not launched as an unsupervised Herdr startup hook.

### Configure Herdr

Merge these entries into your existing Herdr config. **Do not replace your entire config or create a second `[keys]` table.**

```toml
[keys]
previous_agent = "alt+c"
next_agent = "alt+t"

[[keys.command]]
key = "alt+h"
type = "plugin_action"
command = "local.agent-history.previous"
description = "Focus previous visited agent"

[[keys.command]]
key = "alt+n"
type = "plugin_action"
command = "local.agent-history.next"
description = "Focus next visited agent or unseen completion"
```

Remove any older bindings that claim the same chords, then apply the configuration:

```sh
herdr server reload-config
```

Check outer-terminal and desktop shortcuts too. For example, a Ghostty binding that scrolls on Alt+C will consume the key before Herdr receives it.

### First-use check

History starts at the selected agent when the tracker connects; earlier visits cannot be reconstructed.

1. Use Alt+C/T to visit a different agent.
2. Press Alt+H to return to the previous one.
3. Press Alt+N to move forward again.
4. At the end of the history, press Alt+N to visit an unseen completion, if one is waiting; otherwise it wraps.

## How it works

One tracker consumes Herdr's ordered `events.subscribe` stream and observes ordinary focus changes, including mouse clicks and native navigation. Short-lived plugin actions send requests over a private Unix socket. Only the tracker mutates history and issues focus requests.

The tracker:

- Subscribes before obtaining a bootstrap snapshot.
- Uses terminal IDs to retain identity across pane moves where possible.
- Plans a navigation change on a copy, then commits it after a successful focus request.
- Suppresses expected echoes of its own focus requests instead of recording them as new ordinary visits.
- Remembers visited state-change sequences so an already-viewed server completion is not taken in again. A later completion with a newer sequence is eligible.
- Saves state atomically with owner-only permissions.
- Rejects expired queued navigation requests and requests for a different Herdr socket.

**It calls Herdr's `pane.focus` API. It does not send keys, `/tree` commands, or other input into Pi or another coding agent.** Resetting this plugin's history does not change an agent's conversation history.

## Known limitations—where the vibe slop shows

This is a useful personal prototype, not a claim that Herdr's API can provide perfect client-local history.

### Focus attribution is heuristic

Herdr's public focus events do not include a client ID or request correlation. The tracker uses a short-lived pending-target queue to recognize its own focus echoes. An external focus to the same target can be ambiguous.

Do not expect independent histories across multiple attached clients, saved remote machines, or separate Herdr sessions. Those need additional API support and implementation work.

### Completion dots can disagree

Completion selection uses the server's `done` status, supplemented by the tracker’s observed state-change acknowledgements. A particular client's filled completion dots can differ from server state. The tracker cannot inspect that client's private presentation state directly.

### Missing events mean missing visits

After a disconnect, the tracker reconnects, prunes stale entries, and reconciles current focus. It cannot reconstruct intermediate visits made while disconnected. Navigation is disabled while the stream is disconnected.

Herdr 0.9.1 also has known event-burst delivery limitations fixed in 0.9.2. Prefer 0.9.3 or newer. The plugin cannot detect every event silently dropped by an older server.

### Arrival order is not guaranteed keystroke order

Herdr launches plugin action processes asynchronously. The tracker serializes requests in IPC arrival order, which can differ from physical keypress order under extreme scheduling delays.

If something behaves strangely, inspect the diagnostics rather than assuming an exit code of zero means the navigation matched your intent.

## Diagnostics

```sh
~/code/herdr-agent-history/herdr-agent-history inspect
systemctl --user status herdr-agent-history.service
journalctl --user -u herdr-agent-history.service -n 30 --no-pager
herdr plugin log list --plugin local.agent-history
```

Inspection reports:

- The visited terminal IDs and current cursor.
- Live pane mappings and the tracker’s known agent states.
- Acknowledged state-change sequences.
- Connection state and observed gaps.
- The last navigation decision, including reasons such as `history_next`, `completion`, and `history_wrap_next`.

State and the control socket live under:

```text
~/.local/state/herdr/plugins/local.agent-history/
```

The plugin stores identifiers and navigation diagnostics, **not conversation text**. It is ordinary local code running as your user, with access to Herdr's API—not a sandboxed extension.

### Reset

Discard the visited list and start from the current agent:

```sh
~/code/herdr-agent-history/herdr-agent-history reset
```

Corrupt or incompatible persisted state is an explicit startup error. Stop the service, move `history.json` aside, and restart it. Do not edit the state file while the tracker runs.

## Development and verification

```sh
go test -race ./...
go vet ./...
go test -count=20 ./internal/tracker ./internal/herdr
```

Tests cover history insertion and revisits, wrapping, forward-tail priority, completion intake and acknowledgement, identity across pane moves, failed focus, ordered event delivery, cancellation, IPC, state persistence, and expired requests. The history reducer also has fuzz tests.

An optional **read-only** integration probe checks a live Herdr snapshot and subscription handshake. It does not focus a pane or acknowledge a completion:

```sh
HERDR_HISTORY_LIVE_SOCKET="$HOME/.config/herdr/herdr.sock" \
  go test ./internal/herdr -run TestReadOnlyLiveHerdr -v
```

After rebuilding a running installation, restart the tracker:

```sh
go build -trimpath -o herdr-agent-history ./cmd/herdr-agent-history
systemctl --user restart herdr-agent-history.service
```

## Disable or roll back

```sh
systemctl --user disable --now herdr-agent-history.service
herdr plugin disable local.agent-history
```

Restore your previous Alt+H/N bindings and run `herdr server reload-config`. Alt+C/T remain native Herdr actions. Disabling the plugin does not delete its history or alter any agent conversation.

## Credits and license

Inspired by [choplin/herdr-next-agent](https://github.com/choplin/herdr-next-agent) and built around [Herdr's public plugin and socket APIs](https://herdr.dev/docs/plugins/).

This is an unofficial personal workaround, not affiliated with or endorsed by Herdr. Improvements, bug reports, and less-sloppy implementations are welcome.

[MIT license](LICENSE).
