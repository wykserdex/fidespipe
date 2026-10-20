#!/usr/bin/env python3
"""iban_validator — проверка IBAN (ISO 13616): синтаксис, страна, длина, контроль mod-97.

Протокол fidespipe: stdin <- JSON со входом, stdout -> ровно один JSON-конверт.
exit 0 — успех, 1 — доменная ошибка (результат отрицательный), 2 — платформенная.
Сеть и секреты не нужны — permissions честно пустые.
"""
import json
import re
import sys

# Длина IBAN по странам (ISO 13616)
COUNTRY_LEN = {
    "RU": 33, "DE": 22, "GB": 22, "FR": 27, "IT": 27, "ES": 24,
    "NL": 18, "PL": 28, "KZ": 20, "UA": 29, "CH": 21, "AT": 20,
    "BE": 16, "FI": 18, "SE": 24, "CZ": 24, "EE": 20, "LT": 20,
    "LV": 21, "RO": 24,
}

COUNTRY_NAME = {
    "RU": "Россия", "DE": "Германия", "GB": "Великобритания",
    "FR": "Франция", "IT": "Италия", "ES": "Испания", "NL": "Нидерланды",
    "PL": "Польша", "KZ": "Казахстан", "UA": "Украина", "CH": "Швейцария",
    "AT": "Австрия", "BE": "Бельгия", "FI": "Финляндия", "SE": "Швеция",
    "CZ": "Чехия", "EE": "Эстония", "LT": "Литва", "LV": "Латвия",
    "RO": "Румыния",
}

IBAN_RE = re.compile(r"^[A-Z]{2}[0-9]{2}[A-Z0-9]{11,30}$")


def ok(output):
    print(json.dumps({"status": "ok", "output": output}, ensure_ascii=False))
    return 0


def fail(code, message, retryable=False):
    print(json.dumps({"status": "error",
                      "error": {"code": code, "message": message,
                                "retryable": retryable}},
                     ensure_ascii=False))
    return 1


def platform_bad_input(message):
    # битый stdin / неверные типы — платформенная ошибка, ран стопится
    print(json.dumps({"status": "error", "error": {
        "code": "bad_input", "message": message, "retryable": False}},
        ensure_ascii=False))
    return 2


def mod97(iban):
    """ISO 7064 mod-97: первые 4 символа в конец, буквы -> 10..35."""
    rearranged = iban[4:] + iban[:4]
    n = 0
    for ch in rearranged:
        if ch.isdigit():
            n = (n * 10 + int(ch)) % 97
        else:
            n = (n * 100 + ord(ch) - 55) % 97
    return n


def main():
    try:
        data = json.load(sys.stdin)
    except Exception as e:  # noqa: BLE001
        return platform_bad_input("stdin не является JSON: %s" % e)

    # Guard по типам: контракт проверяем сами, не гадаем про вход
    if not isinstance(data, dict) or not isinstance(data.get("iban"), str):
        return platform_bad_input("ожидался объект {iban: string}")

    raw = data["iban"].strip()
    if not raw:
        return fail("empty_input", "поле iban пустое")

    iban = re.sub(r"[\s\-.]", "", raw).upper()

    if not IBAN_RE.match(iban):
        return fail(
            "not_iban",
            "строка не похожа на IBAN (2 буквы страны + 2 контрольные цифры + 11..30 символов)")

    country = iban[:2]
    if country not in COUNTRY_LEN:
        supported = ", ".join(sorted(COUNTRY_LEN))
        return fail("unsupported_country",
                    "страна %s не поддерживается (поддерживаются: %s)"
                    % (country, supported))

    expected = COUNTRY_LEN[country]
    if len(iban) != expected:
        return fail("bad_length",
                    "длина %d, а для %s ожидается %d" % (len(iban), country, expected))

    checksum = mod97(iban)
    if checksum != 1:
        return fail("bad_checksum",
                    "контрольная сумма mod-97 = %d, ожидалось 1" % checksum)

    bban = iban[4:]
    return ok({
        "valid": True,
        "country_code": country,
        "country_name": COUNTRY_NAME[country],
        "normalized": iban,
        "formatted": " ".join(iban[i:i + 4] for i in range(0, len(iban), 4)),
        "checksum": iban[2:4],
        "checksum_ok": True,
        "bban": bban,
        "bank_code": bban[:4],
    })


if __name__ == "__main__":
    sys.exit(main())
