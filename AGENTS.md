# Quickworks Agent Context

## Confirmed Decisions

- Development public URL: `http://localhost:8082`
- Server listen address: `127.0.0.1:8082`
- Workspace access mode: path-based at `/w/{workspace_id}`
- Incus storage and networking: use the existing `default` profile
- GitHub repository support: public and private repositories
- GitHub authentication: OAuth App only; do not add GitHub App support
- GitHub OAuth scopes: `read:user` and `repo`
- GitHub OAuth App name: `github quick works oauth`
- GitHub OAuth Client ID: `Ov23lirP7GjgRF44SY7o`
- GitHub OAuth callback URL: `http://localhost:8082/auth/github/callback`
- Allowed GitHub numeric user ID: `22955496`
- Workbench BYOK provider type: native `deepseek`
- Workbench BYOK base URL: `https://api.deepseek.com/anthropic`
- Workbench model: `deepseek-v4-flash`
- Workbench model context window: `1000000` tokens
- Workbench model maximum output: `65536` tokens
- Workbench API key environment variable: `DEEPSEEK_API_KEY`
- Workbench BYOK catalogue: `workbench-byok.json`
- Workbench process environment: administrator-defined dotenv file `.secrets/workbench.env`
- Workspace auto-stop interval: `8h`
- OpenTofu-compatible CLI: `/usr/bin/terraform` (`Terraform v1.11.4` verified)
- Template location during development: repository-local `templates/incus-vm-v1`
- Templates are administrator-defined and selectable by name; the default template is used when none is requested
- A requested template may require multiple provisioner labels, all of which must match a provisioner
- Phase 3 focus: multi-provisioner scheduling and quotas; Alibaba Cloud pay-as-you-go ECS support is deferred to a later provider-expansion phase
- Alibaba Cloud ECS development region: `cn-hangzhou`, availability zone: `cn-hangzhou-b`
- Alibaba Cloud ECS development VPC ID: `vpc-bp11gtjql4ww5uvebv4b5`
- Alibaba Cloud ECS development vSwitch ID: `vsw-bp130rpt8bj4j0ze3lbtv`
- Alibaba Cloud ECS development security group ID: `sg-bp1fcya0x72n12x6y28u`
- Workspace IDs are immutable three-part pet names such as `calm-blue-harbor`

## Local Secrets

- GitHub OAuth Client Secret: `.secrets/github-oauth-client-secret`
- Workbench environment, including `DEEPSEEK_API_KEY`: `.secrets/workbench.env`
- Never print, copy into documentation, commit, or pass this value as a command-line argument.
- The GitHub OAuth secret was exposed in chat on 2026-07-30, but the user explicitly accepted this for the development-only, non-production OAuth App. Do not block development on rotation; rotate before any production use.
- The Workbench env file is populated locally and has mode `0600`.
- Other required secret files have not been created yet: Quickworks server key and provisioner token.

## Verified Local Environment

- Linux amd64
- Go `1.24.0`
- Terraform `1.11.4`
- GitHub CLI `2.45.0`, authenticated as numeric user ID `22955496`
- Incus server is reachable; the `default` profile uses storage pool `default` and network `incusbr0`
- `/dev/kvm` is available and `images:ubuntu/24.04/cloud` is reachable
- The Incus template passes `terraform validate`

## Implementation Constraints

- Use one pure-Go `quickworks` binary with `server`, `provisioner`, and `agent` subcommands.
- Keep server and provisioner deployable as separate processes and hosts.
- Use GORM with `github.com/glebarez/sqlite`; all Go tests and builds must pass with `CGO_ENABLED=0`.
- Server and provisioner use separate YAML files. The control plane selects a configured template name; the provisioner loads it from its configured `provisioner.template_dirs` mapping only after its labels satisfy that template's requirements.
- Workbench is the external `hempflower/vscode-agents-server` release and is downloaded by the agent from the control-plane-provided URL.
- Parse `workbench.env_file` as dotenv data without shell evaluation and use it as the `vscode-agents-server` process environment.
- Do not commit files below `.secrets/`, Terraform state, plans, `.tfvars`, or `.terraform/` provider caches.
- Read `ARCHITECTURE.md` before implementing behavior; update it when a confirmed architectural decision changes.

## Next External Inputs

- None. Development can proceed without additional user-provided inputs.
