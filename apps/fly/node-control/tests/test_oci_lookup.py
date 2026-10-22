"""Unit tests for oci._instance_id (name lookup vs pinned OCID), no OCI SDK or network needed."""
import importlib
import os
import sys
import types
import unittest
from types import SimpleNamespace as NS

import _helpers  # noqa: F401  (sys.path)
from app.errors import APIError, NotConfigured


def load_oci_module(instances):
    sys.modules["oci"] = types.ModuleType("oci")  # the real SDK is not needed for these tests
    sys.modules.pop("app.oci", None)
    mod = importlib.import_module("app.oci")
    calls = []

    class FakeClient:
        def list_instances(self, compartment_id, display_name):
            calls.append((compartment_id, display_name))
            return NS(data=[i for i in instances if i.display_name == display_name])

    mod._client = lambda region: FakeClient()
    return mod, calls


def inst(id_, state, name="web"):
    return NS(id=id_, lifecycle_state=state, display_name=name)


def node(**block):
    return {"provider": "oci", "oci": {"region": "r1", **block}}


class InstanceId(unittest.TestCase):
    def test_pinned_ocid_wins_and_skips_lookup(self):
        m, calls = load_oci_module([])
        self.assertEqual(m._instance_id(node(instance_id="pinned", compartment_id="c", instance_name="web")), "pinned")
        self.assertEqual(calls, [])

    def test_finds_live_instance_by_name(self):
        m, calls = load_oci_module([inst("new", "RUNNING"), inst("old", "TERMINATED")])
        self.assertEqual(m._instance_id(node(compartment_id="comp", instance_name="web")), "new")
        self.assertEqual(calls, [("comp", "web")])

    def test_survives_replacement(self):
        n = node(compartment_id="c", instance_name="web")
        m, _ = load_oci_module([inst("v1", "TERMINATING")])
        with self.assertRaises(APIError):
            m._instance_id(n)  # only the dying instance exists
        m, _ = load_oci_module([inst("v1", "TERMINATING"), inst("v2", "PROVISIONING")])
        self.assertEqual(m._instance_id(n), "v2")

    def test_ambiguous_refuses(self):
        m, _ = load_oci_module([inst("a", "RUNNING"), inst("b", "STOPPED")])
        with self.assertRaises(APIError):
            m._instance_id(node(compartment_id="c", instance_name="web"))

    def test_not_configured(self):
        m, _ = load_oci_module([])
        for n in ({"provider": "oci"}, node(), node(compartment_id="c")):
            with self.assertRaises(NotConfigured):
                m._instance_id(n)

    def test_region_falls_back_to_env(self):
        m, _ = load_oci_module([])
        os.environ["OCI_REGION"] = "from-env"
        try:
            self.assertEqual(m._region({"provider": "oci", "oci": {}}), "from-env")
            self.assertEqual(m._region(node()), "r1")
        finally:
            os.environ.pop("OCI_REGION")
        with self.assertRaises(NotConfigured):
            m._region({"provider": "oci", "oci": {}})


if __name__ == "__main__":
    unittest.main()
