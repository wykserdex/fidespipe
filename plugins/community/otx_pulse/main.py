#!/usr/bin/env python3
"""otx_pulse — публичные пульсы AlienVault OTX по индикатору (паттерн C, urllib).

Вход (stdin JSON): indicator (домен/хост, IP, URL, md5/sha1/sha256),
wall_timeout (опц., общий лимит рана, 60).

Сеть делает только ДОЧЕРНИЙ процесс: тот же интерпретатор с -c SNIPPET (или
OTX_PULSE_BIN, если он задан — единственный путь для тестов). Сниппет делает
GET https://otx.alienvault.com/api/v1/indicators/<type>/<indicator>/general и
печатает в stdout {"indicator_type": <type>, "raw": <ответ>}. Тип индикатора он
определяет сам: ip → IPv4/IPv6, hex-хэш → file, схема http(s) → url, иначе
hostname. Публичные пульсы API отдаёт без ключа (проверено), поэтому секретов
нет. /api/v1/search/pulses без ключа отвечает 403 — этот путь не используется.

Ответ: {"indicator": ..., "type": ..., "pulse_info": {"count": N, "pulses":
[{id, name, created, modified, ...}]}}. В выход идут только id/name/created.

Выход (stdout JSON): {indicator, pulses[{id,name,created}], count, found}.
Индикатор без пульсов (count 0, pulses []; HTTP 404 — «нет такого индикатора»)
— это ok с пустым массивом и found:false, а НЕ ошибка. Доменные ошибки:
empty_indicator, bad_indicator, otx_pulse_not_installed, timeout (retryable),
no_report, tool_failed. Платформенные (exit 2): битый JSON входа,
нечитаемый/не-JSON ответ донора.
"""
import json
import os
import shutil
import subprocess
import sys
import tempfile

DEFAULT_WALL = 60
MAX_INDICATOR = 512

SNIPPET = (
    "import ipaddress, json, os, re, sys, urllib.error, urllib.parse\n"
    "import urllib.request\n"
    "API = 'https://otx.alienvault.com/api/v1/indicators/'\n"
    "indicator = sys.argv[1]\n"
    "timeout = float(sys.argv[2]) if len(sys.argv) > 2 else 30.0\n"
    "def otx_type(value):\n"
    "    try:\n"
    "        ipaddress.ip_address(value)\n"
    "        return 'IPv6' if ':' in value else 'IPv4'\n"
    "    except ValueError:\n"
    "        pass\n"
    "    if re.fullmatch(r'[0-9a-fA-F]{32}|[0-9a-fA-F]{40}|[0-9a-fA-F]{64}',"
    " value):\n"
    "        return 'file'\n"
    "    if value.lower().startswith(('http://', 'https://')):\n"
    "        return 'url'\n"
    "    return 'hostname'\n"
    "kind = otx_type(indicator)\n"
    "url = API + kind + '/' + urllib.parse.quote(indicator, safe='') + "
    "'/general'\n"
    "req = urllib.request.Request(url, headers={'User-Agent': "
    "'fidespipe-otx-pulse/0.1'})\n"
    "try:\n"
    "    with urllib.request.urlopen(req, timeout=timeout) as resp:\n"
    "        text = resp.read().decode('utf-8', 'replace')\n"
    "except urllib.error.HTTPError as exc:\n"
    "    print(json.dumps({'indicator_type': kind, 'http_status': exc.code,"
    " 'error': 'http'}))\n"
    "    sys.exit(1)\n"
    "try:\n"
    "    parsed = json.loads(text)\n"
    "except ValueError:\n"
    "    print(json.dumps({'indicator_type': kind, 'error': 'not_json'}))\n"
    "    sys.exit(1)\n"
    "print(json.dumps({'indicator_type': kind, 'raw': parsed}))\n"
)


try:
    sys.stdin.reconfigure(encoding="utf-8")
    sys.stdout.reconfigure(encoding="utf-8")
except Exception:
    pass


def ok(output):
    print(json.dumps({"status": "ok", "output": output}, ensure_ascii=False))
    return 0


def fail(code, message, retryable=False, exit_code=1):
    print(json.dumps({"status": "error",
                      "error": {"code": code, "message": message,
                                "retryable": retryable}},
                     ensure_ascii=False))
    return exit_code


def build_cmd(indicator, wall):
    bin_env = os.environ.get("OTX_PULSE_BIN", "").strip()
    if bin_env:
        # имя из PATH ищем which'ем, путь приводим к абсолютному
        if "/" in bin_env or "\\" in bin_env:
            cmd = [os.path.abspath(bin_env)]
        else:
            cmd = [shutil.which(bin_env) or os.path.abspath(bin_env)]
        if len(cmd) == 1 and cmd[0].lower().endswith(".py"):
            # .py-мок запускаем через интерпретатор — прямой exec непереносим
            cmd = [sys.executable] + cmd
    else:
        cmd = [sys.executable, "-c", SNIPPET]
    return cmd + [indicator, str(wall)]


def read_envelope(stdout):
    raw = (stdout or "").strip()
    if not raw:
        return "empty", None
    try:
        envelope = json.loads(raw)
    except Exception:
        return "bad", None
    if not isinstance(envelope, dict):
        return "bad", None
    return "ok", envelope


def main():
    try:
        data = json.load(sys.stdin)
    except Exception as e:
        return fail("bad_input", f"невалидный JSON на входе: {e}", exit_code=2)
    if not isinstance(data, dict):
        return fail("bad_input", "ожидается JSON-объект на входе", exit_code=2)

    indicator = str(data.get("indicator") or "").strip()
    if not indicator:
        return fail("empty_indicator", "indicator пуст")
    if len(indicator) > MAX_INDICATOR or any(
            ch.isspace() or ord(ch) < 0x21 for ch in indicator):
        return fail("bad_indicator",
                    f"некорректный индикатор (пробелы/управляющие символы или "
                    f"длиннее {MAX_INDICATOR}): {indicator[:64]!r}")

    try:
        wall = float(data.get("wall_timeout") or DEFAULT_WALL)
    except (TypeError, ValueError):
        return fail("bad_wall_timeout", "wall_timeout обязан быть числом",
                    exit_code=2)

    cmd = build_cmd(indicator, wall)
    with tempfile.TemporaryDirectory() as td:
        try:
            proc = subprocess.run(cmd, capture_output=True, text=True,
                                  cwd=td, timeout=wall)
        except FileNotFoundError:
            return fail("otx_pulse_not_installed",
                        "донор OTX Pulse не найден (укажите OTX_PULSE_BIN)")
        except subprocess.TimeoutExpired:
            return fail("timeout",
                        f"OTX Pulse не уложился в {wall:.0f}s: увеличьте "
                        "wall_timeout", retryable=True)
        if proc.stderr:
            sys.stderr.write(proc.stderr)

        shape, envelope = read_envelope(proc.stdout)
        if proc.returncode != 0:
            if envelope is None:
                tail = (proc.stderr or "").strip().splitlines()
                last = tail[-1] if tail else f"exit {proc.returncode}"
                network = ("URLError" in last or "timed out" in last
                           or "Connection" in last)
                return fail("tool_failed",
                            f"запрос к OTX не удался: {last}", retryable=network)
            status = envelope.get("http_status")
            if status == 404:
                return ok({"indicator": indicator, "pulses": [], "count": 0,
                           "found": False})
            if status:
                return fail("tool_failed",
                            f"OTX вернул HTTP {status}",
                            retryable=str(status).startswith("5"))
            return fail("tool_failed", "донор OTX вернул пустой ответ")

    if shape == "empty":
        return fail("no_report", "дочерний процесс не вернул JSON в stdout")
    if shape == "bad" or envelope is None:
        return fail("bad_report", "ответ OTX не JSON", exit_code=2)
    if envelope.get("error") == "not_json":
        return fail("bad_report", "OTX ответил не-JSON", exit_code=2)

    response = envelope.get("raw")
    if not isinstance(response, dict):
        return fail("bad_report", "неожиданный формат ответа OTX", exit_code=2)
    if response.get("error"):
        return fail("tool_failed",
                    f"OTX вернул ошибку: {response.get('error')}", retryable=True)

    pulse_info = response.get("pulse_info")
    if not isinstance(pulse_info, dict):
        pulse_info = {}
    raw_pulses = pulse_info.get("pulses")
    if not isinstance(raw_pulses, list):
        raw_pulses = []

    pulses = []
    for item in raw_pulses:
        if not isinstance(item, dict):
            continue
        pulses.append({"id": str(item.get("id") or ""),
                       "name": str(item.get("name") or ""),
                       "created": str(item.get("created") or "")})

    return ok({"indicator": indicator, "pulses": pulses,
               "count": len(pulses), "found": bool(pulses)})


if __name__ == "__main__":
    sys.exit(main())