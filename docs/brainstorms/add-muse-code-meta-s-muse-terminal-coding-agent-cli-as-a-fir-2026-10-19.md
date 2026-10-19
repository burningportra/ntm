# Brainstorm — Add Muse Code as a first-class ntm agent type

**Date:** 2026-10-19
**Goal slug:** add-muse-code-meta-s-muse-terminal-coding-agent-cli-as-a-fir
**Source:** Phase 0.5 pressure-test (skills/start/_planning.md §4.5)

## Framing synthesis

Ship Muse Code (Meta's terminal coding agent, binary `muse`, v1.4.11 installed at `~/.local/bin/muse`) as a peer of cc/cod/gmi/pi/kimi with full parity to the Kimi integration in commit a4c1265f: agent type and detection, `--muse` on spawn/add/send/adopt, `--spawn-muse` in robot mode, config command template, model registry entry, robot health/dashboard/capabilities/markdown/tokens, and docs. The floor is that full parity, not a thinner slice. The ceiling is breadth: Muse appearing everywhere other agents do (ensembles, swarm mixes, assign capability scores, pipelines, checkpoint/restore, context tracking), which goes into a future-direction appendix unless a surface is already covered by the Kimi pattern.

## User answers

### Smallest version (scope floor)
- **Selected:** Full Kimi parity
- **Detail:** Mirror the Kimi commit end to end: agent type + detection, --muse on spawn/add/send/adopt, --spawn-muse robot flag, config, model registry, robot health/dashboard/capabilities, docs.

### 10x version (ambition ceiling)
- **Selected:** Breadth expansion
- **Detail:** Muse everywhere other agents are: ensembles, swarm mixes, assign capability scores, pipelines, checkpoint/restore, context tracking.

### Adjacent asks (scope creep radar)
- **Selected:** Nothing adjacent
- **Detail:** Muse support is isolated; no related asks to fold in. Deduplicating the per-agent-type switch statements is an explicit non-goal.

## Muse Code facts the planner needs

- Binary: `muse`. Interactive TUI by default; `muse "<prompt>"` starts a session with a prompt; `muse exec` is headless; `muse resume --last` resumes.
- Flags relevant to a launch template: `--model <MODEL>`, `--reasoning-effort none|minimal|low|medium|high|xhigh|max|ultra` (default high), `-w/--worktree off|create|existing` (default off, so ntm's shared checkout is untouched), `--provider meta|local|echo`.
- Model: Muse Spark 1.3 (1M-token context per public docs). Auth via Meta developer login or `META_API_KEY`.
- First run asks whether to trust the workspace, so detection patterns should recognize that prompt.

## Planner instructions

Anchor the plan to the smallest version. Put the 10x version in a future-direction appendix. Include adjacent work only when the user selected "Bundle a related ask"; otherwise list it under explicit non-goals.
