# zot-editor-bridge

A zot extension that exposes the active zot session to external editors over a
localhost HTTP API. Select code in Sublime Text, VS Code, Neovim, or any script,
send it with an instruction, and zot receives it as a normal user prompt.

- Binds to `127.0.0.1` only.
- No external Go dependencies, standard library only.
- Publishes its port so editors can discover the running session.
- Falls back to an ephemeral port when the default one is taken, so multiple
  zot windows can run side by side.

## Supported editors

- [ZotInstruct](https://github.com/unlight/ZotInstruct) for Sublime Text

Any editor or script that can send JSON over HTTP can integrate directly; see
[HTTP API](#http-api).

## Requirements

- zot with extension support (tested with zot 0.4.78).
- Go 1.22 or newer on `PATH`.

The manifest runs the extension with `go run .`, so `zot ext install .` is enough
on a machine with a Go toolchain. Build a standalone binary if you prefer:

```bash
go build -o zot-editor-bridge .
```

Then change `exec` in `extension.json` to `"./zot-editor-bridge"` and drop the
`args` entry.

## Install

```bash
zot ext install https://github.com/unlight/zot-editor-bridge.git
```

zot clones the repository into `$ZOT_HOME/extensions/zot-editor-bridge/`.
The manifest runs `go run .`, so Go 1.22 or newer must stay available on `PATH`.

Restart zot. A startup note reports the listening port, and `/editor-bridge`
shows the current `/send` and `/health` URLs, the session `cwd`, and the port
file path.

## Port discovery

On startup the extension tries to bind `127.0.0.1:47831`, unless
`ZOT_EDITOR_BRIDGE_PORT` is set. If the port is busy, it binds an ephemeral port
(`:0`) instead.

The chosen port is written to a file scoped to the session's absolute `cwd`:

```text
editor-bridge-<cwd-hash>.port
```

`<cwd-hash>` is the first 8 lowercase hex characters of
`SHA-256(filepath.Clean(cwd))` encoded as UTF-8. Example: `/work/project` hashes
to `65d80d2c`, so the file is `editor-bridge-65d80d2c.port`.

The file is written to:

1. `$ZOT_HOME/editor-bridge-<hash>.port` when `ZOT_HOME` is set.
2. Otherwise the zot state directory:
   - Linux: `$XDG_STATE_HOME/zot/editor-bridge-<hash>.port`, or
     `~/.local/state/zot/editor-bridge-<hash>.port`
   - macOS: `~/Library/Application Support/zot/editor-bridge-<hash>.port`
   - Windows: `%LOCALAPPDATA%\zot\editor-bridge-<hash>.port`

No other locations are written.

Editors compute the hash from their project root and read that file. The project
root must match zot's `cwd` exactly after `filepath.Clean`; otherwise the hashes
differ and the editor falls back to another directory or reports "zot not
running". The file contains only the decimal port.

To confirm the file is live, call `GET /health` and check that `cwd` matches the
project root. This rejects stale files left by a crashed process and files owned
by a window whose `cwd` changed.

Two zot windows opened in the same `cwd` share one file name: the last started
window wins. Windows opened in different project directories get different
files and are addressed independently.

The file is removed on `shutdown`, `SIGINT`, and `SIGTERM`, but only when it
still contains this process's port. If another zot window has overwritten it,
the file is left alone.

To pin a port:

```bash
ZOT_EDITOR_BRIDGE_PORT=51234 zot
```

## HTTP API

### `GET /health`

```bash
HASH=$(printf %s "$PWD" | sha256sum | cut -c1-8)
PORT=$(cat ~/.local/state/zot/editor-bridge-$HASH.port)
curl http://127.0.0.1:$PORT/health
```

```json
{"cwd":"/work/project","ok":true,"port":47831,"session_id":"LX347S3QQXA2XVCKVEW7K2GLCI"}
```

`session_id` is filled from zot lifecycle events and can be empty briefly during
startup or when the host does not expose one.

### `POST /send`

```bash
curl -X POST http://127.0.0.1:$PORT/send \
  -H 'Content-Type: application/json' \
  -d '{
    "code": "fmt.Println(\"hi\")",
    "instruction": "What does this code do?",
    "file": "main.go",
    "language": "go"
  }'
```

Fields:

| field | required | meaning |
|---|---|---|
| `instruction` | one of `instruction` / `code` | what the user wants |
| `code` | one of `instruction` / `code` | selected code or text |
| `file` | no | file path shown to the agent |
| `language` | no | code fence language |
| `mode` | no | `prompt` (default) or `notify` |

The request is rejected with HTTP 400 when the JSON is malformed, `mode` is
unknown, or both `instruction` and `code` are blank. Bodies over 1 MiB return
HTTP 413.

Response:

```json
{"mode":"prompt","ok":true,"port":47831,"session_id":"LX347S3QQXA2XVCKVEW7K2GLCI"}
```

The extension first pushes an `info` notification, then formats the request as
Markdown and submits it as a user prompt:

````text
File: main.go
Instruction: What does this code do?

```go
fmt.Println("hi")
```
````

If zot is busy, the prompt is queued by the host exactly like typed input.
`"mode": "notify"` sends only the status notification and does not start an
agent turn.

## Protocol

zot protocol v1 expects the extension to send `hello` first, then wait for
`hello_ack`, register commands, and send `ready`. User text is submitted with
the `submit` frame. Session metadata is taken from `hello_ack` and lifecycle
events.

## Development

```bash
gofmt -w .
go vet ./...
go test ./...
```

Install a working copy for a single session without copying it:

```bash
zot --ext ./zot-editor-bridge
```

Logs and diagnostics:

```bash
zot ext doctor
zot ext logs editor-bridge -f
```

## Security notes

The server listens on loopback only and accepts at most 1 MiB per request. It
has no authentication: any local process that can reach the loopback port can
submit a prompt to the agent. Do not bind it to a routable interface.
