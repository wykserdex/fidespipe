#!/usr/bin/env python3
"""Фикстура: отдаёт значение FIDESPIPE_NETWORK из env (контракт declare-now)."""
import json
import os
import sys

data = json.load(sys.stdin)
print(json.dumps({"status": "ok", "output": {"value": os.environ.get("FIDESPIPE_NETWORK", "unset")}}))
