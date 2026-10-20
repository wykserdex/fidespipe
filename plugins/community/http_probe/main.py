#!/usr/bin/env python3
"""http_probe — GET-запрос через urllib: статус, редиректы, размер, время.

MOCK=1 — детерминированный ответ без сети (для тестов/CI).
"""
import json
import os
import sys
import time
import urllib.error
import urllib.request


try:
    sys.stdin.reconfigure(encoding="utf-8")
    sys.stdout.reconfigure(encoding="utf-8")
except Exception:
    pass


def ok(output):
    print(json.dumps({"status": "ok", "output": output}, ensure_ascii=False))
    return 0


def fail(code, message, retryable=False):
    print(json.dumps({"status": "error",
                      "error": {"code": code, "message": message,
                                "retryable": retryable}},
                     ensure_ascii=False))
    return 1


def main():
    try:
        data = json.load(sys.stdin)
    except Exception as e:
        print(json.dumps({"status": "error", "error": {
            "code": "bad_input", "message": str(e), "retryable": False}},
            ensure_ascii=False))
        return 2

    url = str(data.get("url") or "").strip()
    if not url:
        return fail("empty_input", "поле url пустое")
    if not url.startswith(("http://", "https://")):
        return fail("bad_url", "url должен начинаться с http(s)://")
    try:
        timeout = float(data.get("timeout_s") or 10)
    except (TypeError, ValueError):
        return fail("bad_timeout", "timeout_s должен быть числом")
    if timeout <= 0 or timeout > 60:
        return fail("bad_timeout", "timeout_s вне (0, 60]")

    if os.environ.get("MOCK") == "1":
        return ok({"status": 200, "final_url": url,
                   "content_type": "text/html; charset=utf-8",
                   "bytes": 1256, "elapsed_ms": 42, "ok": True})

    req = urllib.request.Request(url, headers={"User-Agent": "fidespipe/http_probe"})
    start = time.monotonic()
    try:
        with urllib.request.urlopen(req, timeout=timeout) as resp:
            body = resp.read(2 * 1024 * 1024)
            elapsed = int((time.monotonic() - start) * 1000)
            status = resp.status
            return ok({
                "status": status,
                "final_url": resp.geturl(),
                "content_type": resp.headers.get("Content-Type", ""),
                "bytes": len(body),
                "elapsed_ms": elapsed,
                "ok": 200 <= status < 300,
            })
    except urllib.error.HTTPError as e:
        return fail(f"http_{e.code}", f"HTTP {e.code} для {url}")
    except Exception as e:
        return fail("network", f"{url}: {e}", retryable=True)


if __name__ == "__main__":
    sys.exit(main())
