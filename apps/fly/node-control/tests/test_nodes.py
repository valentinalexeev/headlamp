"""Tests for app/nodes.py (config validation, UI view, tracked tailnet names)."""
import unittest

import _helpers  # noqa: F401  (sys.path)
from app import nodes

CFG = {
    "nodes": {
        "a": {"provider": "yandex", "tailnetName": "ta", "yandex": {"instance_id": "SECRET-ID"}},
        "b-1": {"provider": "oci", "label": "OCI X", "oci": {"compartment_id": "SECRET-C", "instance_name": "n"}},
        "c": {"provider": "fly", "fly": {"app": "secret-app"}, "stopStatus": "OFF"},
    },
    "tailnetTrack": ["extra"],
}


class Config(unittest.TestCase):
    def test_valid_config_passes(self):
        nodes.validate(CFG)

    def test_rejects_bad_shapes(self):
        for bad in (
            [],
            {},
            {"nodes": []},
            {"nodes": {"x": {"provider": "aws"}}},
            {"nodes": {"X!": {"provider": "fly"}}},          # id must be path-safe
            {"nodes": {"x": {"provider": "fly", "fly": "app"}}},
        ):
            with self.assertRaises(nodes.ConfigError, msg=bad):
                nodes.validate(bad)


class UiView(unittest.TestCase):
    def setUp(self):
        self.view = nodes.ui_view(CFG)

    def test_never_leaks_connection_details(self):
        text = str(self.view)
        for secret in ("SECRET-ID", "SECRET-C", "secret-app", "instance_id", "compartment_id"):
            self.assertNotIn(secret, text)

    def test_provider_defaults_and_overrides(self):
        n = self.view["nodes"]
        self.assertEqual((n["a"]["provider"], n["a"]["startStatus"]), ("Yandex Compute", "RUNNING"))
        self.assertEqual(n["b-1"]["provider"], "OCI X")                       # label override
        self.assertEqual(n["b-1"]["detailFields"], ["detail", "shape"])
        self.assertEqual((n["c"]["startStatus"], n["c"]["stopStatus"]), ("STARTED", "OFF"))
        self.assertTrue(n["c"]["prettyNotConfigured"])

    def test_tailnet_name_defaults_to_id(self):
        n = self.view["nodes"]
        self.assertEqual((n["a"]["tailnetName"], n["b-1"]["tailnetName"]), ("ta", "b-1"))

    def test_title_defaults(self):
        self.assertEqual(self.view["title"], "node-control")

    def test_tracked_names(self):
        self.assertEqual(nodes.tailnet_names(CFG), {"ta", "b-1", "c", "extra"})


if __name__ == "__main__":
    unittest.main()
