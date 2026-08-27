package main

// scaffoldDispatch is .conductor/dispatch.yaml: which concrete models a team wants its work
// sent to, in what order, under which conditions. It is optional — without it, the built-in
// tier router chooses models from models.yaml — but it is where cross-model routing policy
// lives once a team runs more than one model. `conductor policy lint` validates it, and
// `conductor route <task>` previews what it would decide.
const scaffoldDispatch = `# Dispatch policy: send implementation work to models by rule.
#
# Lanes are named execution paths, each an ordered ladder of candidate models. The first
# eligible candidate wins; escalation (on failure) walks down the ladder. Rules pick the lane
# and add floors or preferences for tasks matching a 'when' expression. Everything is
# deterministic: a rule can raise a floor or prefer a model, never route around a hard floor
# the security rules in policies.yaml set.
#
# Facts a 'when' expression can read include: task.labels, task.paths, task.risk,
# task.estimated_files, task.security_sensitive, task.schema_or_migration, attempt.failures,
# budget.fraction, and more — run 'conductor policy lint' to catch typos.

version: 1

lanes:
  implement:
    role: implementer
    candidates:
      # A local model for small, low-risk work — cheap and private. Declare it in models.yaml
      # with a tier so tier floors can still be met. Requires OpenCode + Ollama installed.
      # - model: ollama/qwen3:27b
      #   harness: opencode
      #   tags: [local, cheap]
      #   when: task.estimated_files <= 3 && !task.security_sensitive
      #   max_concurrent: 1
      - model: claude-sonnet-5
        harness: claude
        effort: medium
      - model: claude-opus-5
        harness: claude
        effort: high

  review:
    role: reviewer
    review: required
    candidates:
      - alias: reviewer.strong

rules:
  # Documentation and comment-only work can go to the cheapest local candidate.
  - id: docs-local
    when: task.labels has "docs"
    prefer: { tag: local }

  # Anything touching the model catalog itself gets the frontier model.
  - id: routing-changes
    when: task.paths any "internal/router/**"
    require: { tier: T3 }

defaults:
  lane: implement
  on_failure: escalate    # escalate | retry | stop
  max_escalations: 2

# Cap concurrent attempts on a model across the whole project — a single-GPU local model,
# a rate-limited API key. 0 or omitted means unlimited.
limits:
  # - match: { harness: opencode }
  #   max_concurrent: 1
`
