#!/usr/bin/env python3
"""Exercise both path ownership and Git event semantics used to skip checks."""

import contextlib
import importlib.util
import io
import os
from pathlib import Path
import subprocess
import tempfile
import unittest


spec = importlib.util.spec_from_file_location("ci_scope", Path(__file__).with_name("ci-scope.py"))
scope = importlib.util.module_from_spec(spec)
spec.loader.exec_module(scope)


class ScopeTests(unittest.TestCase):
    def test_path_ownership(self):
        cases = [
            (["README.md", "docs/OPERATIONS.md"], False, True),
            (["AGENTS.md", "SECURITY.md", "NOTICE"], False, False),
            (["site/src/index.astro", "site/package-lock.json"], False, True),
            ([".github/workflows/pages.yml"], False, True),
            ([".github/workflows/ci.yml"], True, True),
            (["scripts/ci-scope.py"], True, True),
            (["cmd/conductor/main.go", "README.md"], True, True),
            (["docs/bounds/frontier.json"], True, True),
        ]
        for paths, go, site in cases:
            with self.subTest(paths=paths):
                self.assertEqual(scope.classify(paths), {"go": go, "site": site})
        for path in ["go.mod", "go.sum", "Makefile", "staticcheck.conf", "docker-compose.yml",
                     ".conductor/WORKFLOW.md", "internal/web/static/main.css",
                     "internal/db/migrations/0014.sql", "internal/foo/testdata/input.md",
                     "internal/integrations/templates/opencode_plugin.js", "scripts/e2e.sh",
                     "integrations/plugin.js", "new-component/input.json"]:
            with self.subTest(path=path):
                self.assertEqual(scope.classify([path]), {"go": True, "site": False})

    def test_events_and_renames(self):
        original = Path.cwd()
        with tempfile.TemporaryDirectory() as directory:
            os.chdir(directory)
            try:
                def git(*args):
                    return subprocess.check_output(["git", *args], stderr=subprocess.DEVNULL).decode().strip()
                def commit(filename, text):
                    Path(filename).parent.mkdir(parents=True, exist_ok=True)
                    Path(filename).write_text(text)
                    git("add", "-A")
                    git("commit", "-qm", "fixture")
                    return git("rev-parse", "HEAD")
                git("init", "-q")
                git("config", "user.email", "ci@example.invalid")
                git("config", "user.name", "CI test")
                base = commit("README.md", "docs\n")
                git("checkout", "-qb", "pr")
                head = commit("docs/guide.md", "guide\n")
                git("checkout", "-qb", "target", base)
                advanced = commit("internal/main.go", "package main\n")
                event = {"pull_request": {"base": {"sha": advanced}, "head": {"sha": head}}}
                self.assertEqual(scope.scope_for_event("pull_request", event), {"go": False, "site": True})
                git("checkout", "-q", "pr")
                Path("internal").mkdir()
                git("mv", "docs/guide.md", "internal/input.md")
                git("commit", "-qm", "rename")
                renamed = git("rev-parse", "HEAD")
                self.assertEqual(set(scope.changed_paths("push", {"before": head, "after": renamed})),
                                 {"docs/guide.md", "internal/input.md"})
                self.assertEqual(scope.scope_for_event("push", {"before": head, "after": renamed},
                                                     include_push_site=True), {"go": True, "site": True})
                self.assertEqual(scope.scope_for_event("merge_group", {"merge_group": {
                    "base_sha": head, "head_sha": renamed}}), {"go": True, "site": True})
                git("rm", "internal/input.md")
                git("commit", "-qm", "delete")
                deleted = git("rev-parse", "HEAD")
                self.assertEqual(scope.scope_for_event("push", {"before": renamed, "after": deleted}),
                                 {"go": True, "site": False})
                for name, event in [("workflow_dispatch", {}), ("pull_request", {}),
                                    ("push", {"before": "0" * 40, "after": deleted}),
                                    ("push", {"before": "f" * 40, "after": deleted}),
                                    ("push", {"before": deleted, "after": deleted})]:
                    with self.subTest(event=name, data=event):
                        with contextlib.redirect_stdout(io.StringIO()):
                            self.assertEqual(scope.scope_for_event(name, event), {"go": True, "site": name != "push"})
                            if name == "push":
                                self.assertEqual(scope.scope_for_event(name, event, include_push_site=True),
                                                 {"go": True, "site": True})
            finally:
                os.chdir(original)


if __name__ == "__main__":
    unittest.main()
