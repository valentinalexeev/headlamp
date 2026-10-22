import os
import sys

sys.path.insert(0, os.path.join(os.path.dirname(__file__), ".."))

from app import nodes, selectel  # noqa: E402


def test_selectel_is_a_valid_provider():
    nodes.validate({"nodes": {"vega": {"provider": "selectel", "selectel": {"region": "region-1", "instance_name": "vega"}}}})


def test_start_is_idempotent_when_running(monkeypatch):
    calls = []
    monkeypatch.setattr(selectel, "_instance_id", lambda node: "id1")
    monkeypatch.setattr(selectel, "_raw_status", lambda node, i: "ACTIVE")
    monkeypatch.setattr(selectel, "_call", lambda *a, **k: calls.append(a))
    assert selectel.start({}) == ("RUNNING", "RUNNING")
    assert calls == []


def test_stop_posts_os_stop(monkeypatch):
    calls = []
    monkeypatch.setattr(selectel, "_instance_id", lambda node: "id1")
    monkeypatch.setattr(selectel, "_raw_status", lambda node, i: "ACTIVE")
    monkeypatch.setattr(selectel, "_call", lambda node, m, path, **k: calls.append((m, path, k)))
    assert selectel.stop({}) == ("RUNNING", "STOPPING")
    assert calls == [("POST", "/servers/id1/action", {"json": {"os-stop": None}})]
