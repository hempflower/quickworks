# Quickworks

Quickworks is a small control plane for personal, Incus-backed development
workspaces. This repository is at the first implementation stage described in
`ARCHITECTURE.md`.

## Current implementation

- One pure-Go `quickworks` binary with `server`, `provisioner`, and `agent`
  subcommands.
- Strict YAML configuration, pure-Go SQLite/WAL initialization, and versioned
  SQL migration.
- GitHub OAuth login with PKCE-capable OAuth configuration, numeric-user
  allowlist enforcement, JWT browser sessions, CSRF protection for writes, and
  AES-GCM encrypted OAuth-token storage using a key derived from
  `server.secret_key_file`.
- Workspace IDs, lifecycle state transitions, single-active-build constraint,
  idempotent create requests, and authenticated provisioner lease endpoints.
- A provisioner that copies the one configured template, keeps an independent
  local state file per workspace, and runs `init`, `plan`, then `apply`.
- Public bootstrap/release endpoints generated from the running Quickworks
  binary for a newly provisioned VM.
- Agent-side Ed25519 identity storage and safe dotenv parsing for the future
  Workbench supervisor, plus verified and atomically installed Workbench
  bundles (including archive path and symlink containment checks).
- One-time, five-minute agent enrollment bound to the claimed workspace build;
  enrollment tokens are hashed in SQLite and cannot be replayed.

Agent enrollment creates an authenticated, reconnecting WebSocket connection.
Browser HTTP and WebSocket requests are multiplexed through it to the agent's
fixed loopback Workbench target. GitHub repository creation resolves canonical
repository metadata through the logged-in user's OAuth credential; a one-time,
encrypted clone credential is passed only during matching agent enrollment and
is deleted before the agent performs its initial clone.

## Development

Create a local configuration from `config.example.yaml`. It references secret
file paths that are deliberately not in the repository. The server fails fast
when required secret files are missing.

```sh
go mod download
make test
make build
./quickworks server --config config.yaml
```

Run the provisioner as a separate process after provisioning its token file:

```sh
./quickworks provisioner --config config.provisioner.yaml
```

For a local-state recovery drill, stop the relevant provisioner first, select
a retained snapshot, then run:

```sh
./quickworks provisioner restore --config config.provisioner.yaml \
  --workspace calm-blue-harbor --snapshot 20260730T120000.000000000Z.tfstate
```

Do not use a shared or network filesystem for the SQLite database or
OpenTofu state directory.
