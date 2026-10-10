"""Claims schema/rendering checks; run with Python's standard unittest runner."""
from __future__ import annotations

import copy
import importlib.machinery
import importlib.util
import json
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest

HERE = Path(__file__).resolve().parent
ROOT = HERE.parent.parent
LOADER = importlib.machinery.SourceFileLoader("render_comparison", str(HERE / "render-comparison"))
SPEC = importlib.util.spec_from_loader(LOADER.name, LOADER)
renderer = importlib.util.module_from_spec(SPEC)
LOADER.exec_module(renderer)


def example_register():
    return {
        "schema_version": 1,
        "gateways": [{"id": "example", "name": "Example gateway", "tier": "harness"}],
        "capabilities": [{"id": "self-hosted", "name": "Self-hosted deployment"}],
        "claims": [{
            "capability": "self-hosted", "gateway": "example", "status": "yes",
            "detail": "A scoped capability with | a pipe.", "source_url": "https://example.test/docs",
            "verified_on": "2026-10-09", "version": "v0.2.0",
        }],
    }


class ClaimsRendererTests(unittest.TestCase):
    def test_optional_qualifier_and_individual_date(self):
        doc = example_register()
        text = renderer.render(doc)
        self.assertIn("| Capability | Status | Qualifier | Detail | Source | Verified on | Version |", text)
        self.assertIn("| Self-hosted deployment | Yes |  |", text)
        self.assertIn(r"with \| a pipe", text)
        self.assertIn("| 2026-10-09 | v0.2.0 |", text)
        for qualifier in ("beta", "enterprise", "license"):
            doc["claims"][0]["qualifier"] = qualifier
            self.assertIn(f"| Yes | {qualifier} |", renderer.render(doc))

    def test_invalid_qualifier_rejected(self):
        for qualifier in ("preview", "", None, ["beta"]):
            doc = example_register()
            doc["claims"][0]["qualifier"] = qualifier
            with self.assertRaises(ValueError):
                renderer.render(doc)

    def test_confirmed_claims_require_primary_source(self):
        doc = example_register()
        doc["claims"][0]["source_url"] = ""
        for status in ("yes", "partial", "no"):
            doc["claims"][0]["status"] = status
            with self.assertRaises(ValueError):
                renderer.render(doc)
        doc["claims"][0]["status"] = "unknown"
        renderer.render(doc)

    def test_invalid_schema_and_dates_rejected(self):
        for field, value in (("status", "present"), ("verified_on", "2026-02-30"),
                             ("verified_on", "today"), ("gateway", "not-in-register")):
            doc = example_register()
            doc["claims"][0][field] = value
            with self.assertRaises(ValueError):
                renderer.render(doc)
        doc = example_register()
        doc["claims"].append(copy.deepcopy(doc["claims"][0]))
        with self.assertRaises(ValueError):
            renderer.render(doc)

    def test_json_schema_qualifiers_match_renderer(self):
        schema = json.loads((ROOT / "docs/comparison/schema.json").read_text())
        self.assertEqual(schema["properties"]["schema_version"]["const"], 1)
        claim_schema = schema["$defs"]["claim"]
        self.assertEqual(set(claim_schema["properties"]["qualifier"]["enum"]), renderer.QUALIFIERS)
        self.assertNotIn("qualifier", claim_schema["required"])

    def test_fallback_matches_pyyaml_for_full_register(self):
        path = str(ROOT / "docs/comparison/claims.yaml")
        doc = renderer.load_yaml(path)
        self.assertEqual(renderer._load_yaml_fallback(path), doc)
        renderer.validate(doc)
        caps = {cap["id"] for cap in doc["capabilities"]}
        self.assertTrue({"self-hosted", "token-rate-limits", "image-generation",
                         "audio-transcriptions", "audio-speech"} <= caps)

    def test_single_comparison_source_and_no_speed_claims(self):
        self.assertFalse((ROOT / "comparison_iter5.md").exists())
        self.assertFalse((ROOT / "comparison_iter7.md").exists())
        self.assertNotIn("/code/.plans/", (ROOT / "bench/README.md").read_text())
        doc = renderer.load_yaml(str(ROOT / "docs/comparison/claims.yaml"))
        for claim in doc["claims"]:
            detail = claim["detail"].lower()
            for wording in ("drop-in", "250+ providers", "full openai parity", "enterprise-ready", "soc 2"):
                self.assertNotIn(wording, detail)
            self.assertNotRegex(detail, r"\b\d+(?:\.\d+)?\s*(?:microseconds?|rps|req/s|x faster)\b")
            self.assertNotRegex(detail, r"\b(?:only|first|unique)\b.{0,60}(?:a2a|token.exchange)")
            self.assertNotRegex(detail, r"(?:a2a|token.exchange).{0,60}\b(?:only|first|unique)\b")

    def test_check_mode_does_not_write_and_detects_stale_output(self):
        with tempfile.TemporaryDirectory() as directory:
            claims = Path(directory) / "claims.yaml"
            output = Path(directory) / "comparison.md"
            claims.write_text("""schema_version: 1
gateways:
- id: example
  name: Example gateway
  tier: harness
capabilities:
- id: self-hosted
  name: Self-hosted deployment
claims:
- capability: self-hosted
  gateway: example
  status: 'yes'
  qualifier: beta
  detail: Example self-hosting
  source_url: https://example.test/docs
  verified_on: '2026-10-09'
  version: v0.2.0
""")
            command = [sys.executable, str(HERE / "render-comparison"),
                       "--claims", str(claims), "--out", str(output)]
            result = subprocess.run(command + ["--check"], capture_output=True, text=True)
            self.assertEqual(result.returncode, 1)
            self.assertFalse(output.exists())
            self.assertEqual(subprocess.run(command, capture_output=True).returncode, 0)
            self.assertEqual(subprocess.run(command + ["--check"], capture_output=True).returncode, 0)
            output.write_text("stale output\n")
            self.assertEqual(subprocess.run(command + ["--check"], capture_output=True).returncode, 1)
            self.assertEqual(output.read_text(), "stale output\n")


if __name__ == "__main__":
    unittest.main()
