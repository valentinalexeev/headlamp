#!/usr/bin/env bash
# Runs the Python unit tests of the per-group xray outbounds (#139) with real ansible.
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")"
python3 -m unittest -v test_group_outbounds.py
