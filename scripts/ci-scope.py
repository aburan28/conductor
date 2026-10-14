#!/usr/bin/env python3
"""Select CI work from the complete event diff; uncertain inputs run all checks."""

import json
import os
from pathlib import Path
import subprocess


PROSE = {"AGENTS.md", "CLAUDE.md", "LICENSE", "NOTICE", "SECURITY.md"}


def classify(paths):
    scope = {"go": False, "site": False}
    for path in paths:
        if path.startswith("site/") or path == "README.md":
            scope["site"] = True
        elif path.startswith("docs/"):
            scope["site"] = True
            # Other files under docs may be machine-readable runtime inputs.
            scope["go"] |= not path.endswith(".md")
        elif path == ".github/workflows/pages.yml":
            scope["site"] = True
        elif path not in PROSE:
            # Includes embedded assets, SQL, fixtures, policy, manifests and unknown paths.
            scope["go"] = True
        if path in {".github/workflows/ci.yml", "scripts/ci-scope.py", "scripts/test-ci-scope.py"}:
            scope["site"] = True
    return scope


def changed_paths(event_name, event):
    if event_name == "pull_request":
        base = event["pull_request"]["base"]["sha"]
        head = event["pull_request"]["head"]["sha"]
        revision = f"{base}...{head}"
    elif event_name == "merge_group":
        base = event["merge_group"]["base_sha"]
        head = event["merge_group"]["head_sha"]
        revision = f"{base}..{head}"
    elif event_name == "push":
        base, head = event["before"], event["after"]
        if not base.strip("0") or not head.strip("0"):
            raise ValueError("initial or deleted ref has no comparable revision")
        revision = f"{base}..{head}"
    else:
        raise ValueError(f"{event_name} requests a full validation")
    # Disabling rename detection includes both old and new paths; NUL handles odd filenames.
    result = subprocess.run(
        ["git", "diff", "--no-renames", "--name-only", "-z", revision, "--"],
        check=True, capture_output=True,
    )
    return [os.fsdecode(path) for path in result.stdout.split(b"\0") if path]


def scope_for_event(event_name, event, include_push_site=False):
    try:
        paths = changed_paths(event_name, event)
        if not paths:
            raise ValueError("empty diff cannot establish an irrelevant change")
        scope = classify(paths)
    except (KeyError, TypeError, ValueError, subprocess.CalledProcessError, OSError) as error:
        print(f"::warning::CI scope is uncertain ({type(error).__name__}); running all checks")
        scope = {"go": True, "site": True}
    # Pages already validates and deploys relevant main pushes. PRs and merge groups
    # need a build here so a broken site is caught before merging, without duplicate pushes.
    if event_name == "push" and not include_push_site:
        scope["site"] = False
    return scope


def main():
    try:
        event = json.loads(Path(os.environ["GITHUB_EVENT_PATH"]).read_text())
    except (KeyError, ValueError, OSError):
        event = {}
    scope = scope_for_event(os.environ.get("GITHUB_EVENT_NAME", "workflow_dispatch"), event,
                            include_push_site=os.environ.get("CI_INCLUDE_PUSH_SITE") == "true")
    output = "".join(f"{key}={str(value).lower()}\n" for key, value in scope.items())
    print(output, end="")
    if os.environ.get("GITHUB_OUTPUT"):
        with open(os.environ["GITHUB_OUTPUT"], "a") as stream:
            stream.write(output)


if __name__ == "__main__":
    main()
