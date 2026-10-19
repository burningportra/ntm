# Plan: Muse Code as a first-class ntm agent type

**Date:** 2026-10-19
**Goal slug:** add-muse-code-meta-s-muse-terminal-coding-agent-cli-as-a-fir
**Brainstorm:** docs/brainstorms/add-muse-code-meta-s-muse-terminal-coding-agent-cli-as-a-fir-2026-10-19.md
**Template commit:** a4c1265f `feat(agent): add Kimi (Moonshot AI) as first-class agent type`

## Goal

Add Muse Code, Meta's terminal coding agent, as a peer of cc/cod/gmi/pi/kimi. An operator can run `ntm spawn proj --muse=2`, `ntm add proj --muse=1:spark`, `ntm send proj --muse "..."`, `ntm adopt proj --muse=3`, and `ntm --robot-spawn=proj --spawn-muse=1`, and every robot surface that reports Kimi (status, detection, health, capabilities, markdown, dashboard, tokens, ack, bulk assign, ensemble spawn) reports Muse the same way.

The scope floor is full parity with the Kimi commit, file for file. Deduplicating the per-agent-type switch statements is a non-goal for this cycle.

## Ground truth about Muse (measured on this machine, 2026-10-19)

These facts come from running `muse` 1.4.11 (build 1.4.11-R6667.1) in a tmux pane with the free `--provider echo` backend, and from the local model catalog at `~/.local/share/muse/model-catalog/`. They override anything in public write-ups.

- **Binary.** `~/.local/bin/muse` is a bash launcher that ends in `exec "$binary" "$@"`, so the running process is `~/.local/bin/muse-bin-1.4.11-R6667.1`. tmux reports `#{pane_current_command}` as `muse-bin-1.4.11` (truncated). The existing `isAgent("muse")` helper in `internal/tmux/session.go` would not match that, so command detection needs an explicit `muse-bin` prefix rule in addition to the exact `muse` basename.
- **Substring hazard.** "muse" appears inside unrelated words (`amuse`, `museum`, `musescore`). Use exact-basename and `muse-bin` prefix rules, never `strings.Contains(cmd, "muse")`, following the `commandExecutableIs(cmd, "grok")` precedent.
- **Trust prompt.** On an untrusted workspace the first screen is `Do you trust this workspace?` with options `1  Trust and continue` / `2  Quit`. `--yolo` or `--trust-workspace` skips it.
- **Idle screen.** Header line `  Muse Code 1.4.11`, then a `Skills: N loaded · ...` line, a `── Voice input (alt(option)+v to start) ──` rule, a composer line `❯`, a rule, and a status bar `  <model> · <cwd> · YOLO`.
- **Working screen.** `◇ Thinking (1s · esc to interrupt)` (the glyph cycles `◇`/`◈`). Assistant output lines start with `◆`.
- **Cross-detection risk.** The composer glyph `❯` and the phrase `esc to interrupt` are also Claude Code's idle and working markers. Output-based detection must test the `Muse Code \d+\.\d+` header (or the status bar) before any Claude pattern, and a test must assert a captured Muse screen is never classified as `cc`.
- **Input.** A single Enter submits, so `NeedsDoubleEnter` stays false. Enter while a turn is running leaves the text as a draft. Ctrl+C clears the draft (`Draft cleared · press ↑ to restore`) and does not quit; Esc interrupts a running turn.
- **Models** (catalog, all with `context_limit` 1,007,997 and `output_limit` 128,000): `muse-wonder-1.0` (catalog default), `muse-wonder-full-1.0` (marked current), `muse-spark-1.3`, `muse-spark-1.2`. Each has a hidden `-contributor` twin. Reasoning tiers: minimal, low, medium, high, xhigh, max (spark-1.2 stops at xhigh).
- **Launch flags.** `--model <ID>`, `--reasoning-effort none|minimal|low|medium|high|xhigh|max|ultra` (default high), `--yolo` (disables approval and sandbox, trusts the workspace, withholds `request_user_input`), `--trust-workspace`, `--disable-approval`, `-w/--worktree` (default off, so the shared checkout is used, which is what ntm wants). There is no system-prompt-file flag.
- **Free test backend.** `muse --provider echo [--echo-delay-ms N]` replies with `◆ echo: <prompt>` and costs nothing, which makes live verification cheap.

## Design decisions

1. **Identity.** `agent.AgentTypeMuse = "muse"`, `tmux.AgentMuse`, `agents.AgentTypeMuse = "muse"`, `cli.AgentTypeMuse = "muse"`, `swarm.AgentMuse = "muse"`. Canonical aliases: `muse`, `muse-code`, `muse_code`, `musecode`, `muse-cli`, `meta-muse`. Bare `meta` is excluded because it is too generic. DisplayName `Muse Code`, ProfileName `Muse`. Pane titles `<session>__muse_<N>`. CLI flag `--muse`, robot flag `--spawn-muse`.
2. **Launch template** (`internal/config/templates.go`):
   `muse --yolo{{if .Model}} --model {{shellQuote .Model}}{{end}}{{if .ReasoningEffort}} --reasoning-effort {{shellQuote .ReasoningEffort}}{{end}}`
   `--yolo` matches what ntm does for every other agent (`--dangerously-skip-permissions`, `--dangerously-bypass-approvals-and-sandbox`, `--yolo`, `--always-approve`) and keeps the pane from stopping at the trust prompt or a clarifying question. `SystemPromptFile` is not wired because Muse has no such flag; persona prompts reach Muse through the repo's AGENTS.md/CLAUDE.md, which Muse reads. `--worktree` is left at its default (off).
3. **Default model.** `models.default_muse = "muse-wonder-1.0"` (the catalog default), with aliases `wonder` → `muse-wonder-1.0`, `wonder-full` → `muse-wonder-full-1.0`, `spark` / `spark-1.3` → `muse-spark-1.3`, `spark-1.2` → `muse-spark-1.2`. An explicit default keeps Agent Mail registration and context tracking working (the Opencode template comment records the `model cannot be empty` failure an empty model causes).
4. **Model registry.** All four model ids at 1,007,997 tokens, alias `muse` → `muse-wonder-1.0`, budget ratio 0.90 like Kimi.
5. **Detection.**
   - tmux command: `commandExecutableIs(cmd, "muse")` or an executable basename starting with `muse-bin`.
   - Output header: `(?m)^\s*Muse Code \d+\.\d+` checked before the Claude header in `DetectAgentType`.
   - Idle: a line that is exactly `❯` (optionally trailing space) in the last lines, plus the status bar shape `· YOLO` / `<model> · <path>`.
   - Working: `Thinking (`, `esc to interrupt` (only counted once the pane is known to be Muse).
   - Rate limit / error: reuse Kimi's generic lists; add `Do you trust this workspace?` as a blocking-prompt marker if the robot layer has a slot for it.
6. **Composer handling** (`internal/tmux/session.go`): `ComposerClearKeys(AgentMuse)` = `{"C-u"}` (Escape would interrupt a running turn), `composerMarkerForAgent(AgentMuse)` = `❯`. Verify both against a live echo pane.

## Alignment record (2026-10-19)

The operator confirmed bead 7 for Muse only (Pi/Kimi restart gap goes to a separate bead), `--yolo` as the launch approval mode, and `muse-wonder-1.0` as the pinned default model. The baseline tree built cleanly (`go build ./cmd/ntm`) before any Muse edits.

## Interaction with in-flight work

`git status` shows uncommitted Opencode (`--oc`) work from 2026-08-10 in `internal/cli/send.go`, `internal/config/config.go`, `internal/models/registry.go`, `internal/models/registry_test.go`, plus unrelated edits in `internal/bv/bv.go`, `internal/cm/client.go`, `internal/cli/spawn_recovery_test.go`. No agent holds reservations on them. Per AGENTS.md these are treated as our own: Muse edits build on top of them, and committing those files will carry the Opencode hunks along. Bead 2 starts by confirming `go build ./cmd/ntm` passes on the current tree so a pre-existing break is not blamed on Muse.

In `send.go` the Opencode work threads a `targetOc` bool through `matchesLegacySendTypeFilterWithOc` and `buildSendTargetDescriptionWithPiAndKimi`. Muse adds `targetMuse` the same way, matching the surrounding code rather than restructuring it.

## Beads

Each bead owns a disjoint file set so beads in the same wave can run in parallel. Every bead adds or extends tests next to the code it touches and runs `go build ./cmd/ntm`, `go test -short` for its packages, and `gofmt -l` on its files.

| # | Bead | Files owned | Depends on |
|---|------|-------------|-----------|
| 1 | **Agent core + tmux detection** | `internal/agent/types.go`, `types_test.go`, `patterns.go`, `patterns_test.go`, `parser.go`, `parser_test.go`, `internal/tmux/session.go`, `internal/tmux/session_test.go` | — |
| 2 | **Models, config, launch template, CLI agent spec** | `internal/models/registry.go`, `registry_test.go`, `internal/config/config.go`, `internal/config/templates.go`, `internal/config/webhooks.go`, config tests, `internal/cli/agent_spec.go` (+ `agent_spec_test.go`) | 1 |
| 3 | **CLI spawn/add/adopt surface** | `internal/cli/spawn.go`, `add.go`, `adopt.go`, `adopt_test.go`, `create.go`, `session_persist.go`, `controller.go`, `deps.go`, `help.go`, `completion_helpers.go`, `profile_switch.go`, `agents_cmd.go`, `internal/session/state.go`, `capture.go`, `restore.go` | 1, 2 |
| 4 | **CLI send + reporting** | `internal/cli/send.go` (+ send tests), `activity.go`, `analytics.go`, `analytics_test.go`, `assign.go`, `dashboard.go`, `internal/output/schema.go`, `internal/output/text.go` | 1, 2 |
| 5 | **Robot mode** | `internal/cli/root.go` (`--spawn-muse`), `internal/robot/spawn.go`, `spawn_test.go`, `detection.go`, `patterns.go`, `capabilities.go`, `health.go`, `agent_health.go`, `markdown.go`, `robot_dashboard.go`, `tokens.go`, `tokens_test.go`, `ack.go`, `bulk_assign.go`, `ensemble_spawn.go`, `docs.go`, `robot.go`, `robot_test.go`, `robot_helpers_pure_test.go` | 1, 2 |
| 6 | **Peer packages** | `internal/agents/profiles.go`, `profiles_test.go`, `internal/assign/capability.go`, `internal/pipeline/parser.go`, `internal/pipeline/schema.go`, `internal/swarm/agent_launcher.go` | 1 |
| 7 | **Restart, restore and TUI surfaces** | `internal/robot/smart_restart.go`, `internal/checkpoint/restore.go`, `internal/cli/session.go`, `internal/tui/theme/semantic.go`, `internal/tui/styles/badges.go`, `internal/context/rotation.go` (+ their tests) | 1, 2 |
| 8 | **Docs** | `README.md`, `references/COMMANDS.md`, `docs/robot-api-design.md`, `AGENTS.md` (`--type` list) | 3, 4, 5 |
| 9 | **Live verification** | none (evidence goes in the bead close reason) | all |

### Bead details

**1. Agent core + tmux detection.** Add `AgentTypeMuse` with the aliases, DisplayName, ProfileName and IsValid entries from decision 1. Add `muse*Patterns` and `museHeaderPattern` in `patterns.go`, a `GetPatternSet` case, and parser dispatch in every switch Kimi appears in (`DetectAgentType`, frequency scoring, metrics, rate limit, working, idle, error, limit and work indicators). Place the Muse header check before Claude's. In `internal/tmux/session.go` add `AgentMuse`, the command-detection rule from decision 5, and the composer keys and marker from decision 6. Tests: alias canonicalization (including `" MUSE-CODE "`), `muse-bin-1.4.11` and `/Users/x/.local/bin/muse-bin-1.4.11-R6667.1` detected as muse, `amuse`, `museum` and `musescore` not detected, the captured idle and working screens above classified as muse and never as cc, `NeedsDoubleEnter` false.

**2. Models, config, launch template, CLI agent spec.** Run `go build ./cmd/ntm` first and record the result. Add the registry entries and budget from decision 4. In `config.go` add `Agents.Muse`, `ModelsConfig.DefaultMuse` / `Muse`, and every Kimi case: model lookup canonicalization, `GetModelName`, the TOML writer, the `agents.*` and `models.*` getters, and the config diff. Add the template from decision 2 and the webhook agent list entry. In `agent_spec.go` add the `AgentTypeMuse` constant and the `cfg.Models.Muse` alias lookup. Tests: `GetModelName("muse", "spark")` = `muse-spark-1.3`, the default resolves to `muse-wonder-1.0`, the rendered template for `Model=muse-spark-1.3, ReasoningEffort=xhigh` equals `muse --yolo --model 'muse-spark-1.3' --reasoning-effort 'xhigh'` (match the repo's `shellQuote` output), and an empty model renders `muse --yolo`.

**3. CLI spawn/add/adopt surface.** Mirror every Kimi hunk in these files: `--muse` flag on spawn, add and adopt; counts in add/create; adopt type spec and counts struct; controller `--agent-type muse`; `deps` entry (`Muse Code`, command `muse`, install hint `curl -fsSL https://dev.meta.ai/install.sh | bash`); help rows; completion; profile switch; session persist and session state (`Muse` count, `MuseCmd`). Tests: spawn plan for `--muse=2` produces titles `proj__muse_1`, `proj__muse_2` and the bead-2 command; adopt `--muse=3` records a muse pane.

**4. CLI send + reporting.** Add `--muse` to `ntm send` with the same `newSendTargetValue` / `NoOptDefVal` pattern, thread `targetMuse` through the type filter, the single-type route recommendation (`routeOpts.AgentType = "muse"`), `hasTypeFilter`, and both target-description builders. Add muse to activity type/color, analytics, assign and dashboard switches and the output schema/text. Tests: `--muse` alone selects only muse panes and describes the target as `muse`; mixed `--cc --muse` selects both.

**5. Robot mode.** Add `MuseCount` / `--spawn-muse` and every Kimi touch in `internal/robot` (dry-run pane plan, name map, launch defaults from `cfg.Agents.Muse`, totals and error text), plus detection, capabilities schema, health, agent health, markdown, dashboard, tokens, ack, bulk assign, ensemble spawn and docs. Tests: `--robot-spawn --spawn-muse=1 --dry-run` JSON lists a muse pane titled `<session>__muse_1`; `--robot-capabilities` lists muse; token estimation uses the 1,007,997 context.

**6. Peer packages.** Muse profile in `agents/profiles.go` (model `muse-wonder-1.0`, budget from the registry, Kimi's specializations), `NormalizeAgentType` / `ParseAgentType` cases, `DefaultCapabilities[tmux.AgentMuse]` with Kimi's scores, pipeline parser/schema type lists, swarm launcher alias, command map and type list. Tests: profile count goes up by one; `GetProfileByName("muse-code")` returns the Muse profile.

**7. Restart, restore and TUI surfaces** (kept in scope at alignment, 2026-10-19). `restartCanonicalAgentType` currently lists neither Pi nor Kimi, so `restartLaunchAlias` maps those panes to `cc` and both `--robot-restart-pane` (`restart_pane.go`) and smart restart relaunch them as Claude. Muse goes into both functions, `restorableAgentCommand` returns `muse`, `cli/session.go` resolves the muse default model and presentation, and the TUI theme and badge get a Muse color and icon. Tests: `restartLaunchAlias("muse") == "muse"`, `restorableAgentCommand` for a muse pane with a shell command returns `muse`.

**8. Docs.** Add `--muse` / `--spawn-muse` rows and examples wherever Kimi appears in README, COMMANDS.md and robot-api-design.md, and add `muse` to the AGENTS.md `--type` list.

**9. Live verification.** With a temporary config that sets `[agents] muse = "muse --provider echo --yolo --echo-delay-ms 3000"` (costs nothing), run `go build -o /tmp/claude-501/.../ntm ./cmd/ntm`, then:
1. `ntm spawn muse-verify --muse=1 --no-user` (or the robot equivalent) and confirm `ntm --robot-status` reports one pane of type `muse` titled `muse-verify__muse_1`.
2. `ntm --robot-send=muse-verify --type=muse --msg='ping'`, then `ntm --robot-is-working=muse-verify` reports working during the 3s delay and idle after.
3. `ntm --robot-tail=muse-verify --lines=10` shows `◆ echo: ping`.
4. `ntm --robot-capabilities | jq` lists muse; `ntm deps` finds `muse`.
5. Kill the session. Record each command and its output in the bead close reason.

## Verification block

- `go build ./cmd/ntm`
- `go test -short ./internal/agent/... ./internal/tmux/... ./internal/models/... ./internal/config/... ./internal/cli/... ./internal/robot/... ./internal/agents/... ./internal/assign/... ./internal/pipeline/... ./internal/swarm/... ./internal/session/... ./internal/output/...`
- `gofmt -l` and `goimports -l` on changed files
- `ubs` on changed files
- Bead 9's live run

## Non-goals

- Deduplicating the per-agent-type switch statements into a registry.
- Driving Muse over MSP (`muse serve`) instead of tmux.
- Reading Muse's event log or `muse trace` for state.
- Fixing Pi and Kimi's absence from smart restart and checkpoint restore (file a bead for it instead).

## Future direction (10x: breadth)

Muse wherever Grok and Opencode already reach beyond Kimi: context rotation and compaction, CASS injection, persona and recipe defaults, ensemble CLI, watch, review queue, logs, mail, rebalance, spawn wizard, dashboard layout and banner, assign effectiveness. A follow-up could also use `muse resume --last` on pane restart so a relaunched Muse pane keeps its session.
