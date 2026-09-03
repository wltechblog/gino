# Gino Installer

A one-command installer that sets up [Gino](https://github.com/wltechblog/gino)
on a fresh Debian-based system, configured for the **root + yolo + brain**
profile: running as root, full unrestricted tool access (`sandbox.mode:
"yolo"`), and local brain embeddings via a Podman-hosted Ollama container.

```bash
curl -fsSL https://raw.githubusercontent.com/wltechblog/gino/main/install/install.sh | sudo bash
```

The script makes no assumptions about what is already installed (beyond apt
being the package manager) and is safe to re-run.

## What it installs

| Component | Detail |
|---|---|
| Base packages | git, curl, ca-certificates, podman (via apt) |
| Go toolchain | 1.26.4 from the official tarball — only if system Go is older than 1.26.3 |
| Gino source | Cloned to `/opt/gino` (updated in place if it already exists) |
| Gino binary | Built with `CGO_ENABLED=0` and vendored dependencies, installed to `/usr/local/bin/gino` |
| Brain (Ollama) | `gino-ollama` Podman container bound to `127.0.0.1:11434` + systemd unit; pulls the `nomic-embed-text` embedding model |

## What it asks

All questions are read from `/dev/tty` (fd 3), so they work even when the
script itself arrives via `curl | bash` — where stdin is the script text, not
your keyboard.

1. **LLM provider** — base URL, API key, and model name (defaults to
   z.ai / `glm-4.6`).
2. **Subagent (optional)** — a dedicated worker agent. If it gets its own
   provider credentials they're stored as a `providers.presets` entry
   (`subagent-provider`) and wired into `spawn.agents`; if you reuse the same
   credentials only the model is overridden.
3. **Channel** —
   - *Telegram*: bot token + numeric `allowFrom` user ID (format-validated),
     installs a `gino-gateway` systemd service ordered after Ollama.
   - *TUI*: no credentials needed; run `gino chat` when installation finishes.

## Where things land

- `/usr/local/bin/gino` — the binary
- `/opt/gino` — cloned source (build directory)
- `/root/.gino/config.json` — generated config (existing configs are backed
  up, and overwrite requires confirmation)
- `gino-ollama` container + systemd unit — brain
- `gino-gateway` systemd unit — Telegram gateway mode only

## Testing without side effects

The script has a test mode used by development:

```bash
GINO_INSTALL_TEST=1 bash install/install.sh
```

with `GINO_INSTALL_{REPO_DIR,GINO_HOME,BIN_DIR,OLLAMA_DATA,UNIT_DIR}`
environment overrides redirecting every write path. In test mode the script
skips apt/podman/systemd entirely — it exercises the prompts and config
generation against a scratch directory.

## Assumptions & caveats

- Debian/Ubuntu or another apt-based distribution. Not tested on RPM distros.
- Runs as root (hence `sudo bash`) — the yolo profile trusts the host.
- If something is already listening on `127.0.0.1:11434`, the Ollama
  container step is skipped (existing Ollama is used as-is).
- Security note: piping scripts from the internet to a root shell executes
  whatever is at that URL. Pin to a known commit hash if that matters to you,
  e.g. `.../gino/<commit-sha>/install/install.sh`.
