"""Генератор баннера Fidespipe.

Запуск: python tools/banner.py

Не магия ради магии: цель — тёмный, аккуратный баннер, который читается как
интерфейс продукта, а не как испорченная картинка. Отсюда решения:

- фон — многооктавный шум (value noise), а не случайные круги: круги читались
  как мусор, шум читается как фактура;
- буквы — чистый набор с трекингом, без «износа» случайными прямоугольниками:
  выглядело бы как сломанный принт, а не как стилизация;
- акцент взят из интерфейса (светлая бирюза), чтобы баннер и продукт были
  из одного материала.
"""

from __future__ import annotations

import os

import numpy as np
from PIL import Image, ImageDraw, ImageFilter, ImageFont

W, H = 1856, 576
OUT = os.path.join(os.path.dirname(__file__), "..", "banner.png")

INK = (238, 243, 245)      # основной текст
ACCENT = (177, 221, 226)   # акцент интерфейса (взят из скриншота GUI)
BG_TOP = (13, 16, 20)
BG_BOTTOM = (5, 6, 9)


def value_noise(rng: np.random.Generator, width: int, height: int, octaves: int = 5) -> np.ndarray:
    """Многооктавный шум в [0,1]."""
    total = np.zeros((height, width), dtype=np.float32)
    amplitude = 1.0
    norm = 0.0
    for octave in range(octaves):
        cells = 2 ** (octave + 2)
        grid = rng.random((cells, cells)).astype(np.float32)
        layer = Image.fromarray((grid * 255).astype(np.uint8)).resize((width, height), Image.BICUBIC)
        total += amplitude * (np.asarray(layer, dtype=np.float32) / 255.0)
        norm += amplitude
        amplitude *= 0.5
    return total / norm


def background(rng: np.random.Generator) -> Image.Image:
    noise = value_noise(rng, W, H, octaves=6)

    # Вертикальный градиент.
    grad = np.linspace(0.0, 1.0, H, dtype=np.float32)[:, None]
    base = np.zeros((H, W, 3), dtype=np.float32)
    for c in range(3):
        base[:, :, c] = BG_TOP[c] * (1 - grad) + BG_BOTTOM[c] * grad

    # Фактура: шум приглушённый и слегка окрашенный в холодные тона.
    texture = (noise - 0.5) * 26.0
    base += texture[:, :, None] * np.array([0.85, 0.95, 1.0], dtype=np.float32)

    # Мягкое свечение акцентом за словом — баннер перестаёт быть плоским.
    yy, xx = np.mgrid[0:H, 0:W].astype(np.float32)
    glow = np.exp(-(((xx - W / 2) / (W * 0.42)) ** 2 + ((yy - H * 0.5) / (H * 0.55)) ** 2))
    base += glow[:, :, None] * np.array(ACCENT, dtype=np.float32) * 0.10

    # Виньетка.
    r = np.sqrt(((xx - W / 2) / (W / 2)) ** 2 + ((yy - H / 2) / (H / 2)) ** 2)
    vig = np.clip(1.0 - np.clip(r - 0.55, 0, None) * 1.5, 0, 1)
    base *= vig[:, :, None]

    # Зерно.
    base += rng.normal(0, 2.4, (H, W, 1)).astype(np.float32)
    return Image.fromarray(np.clip(base, 0, 255).astype(np.uint8))


def draw_tracked(draw: ImageDraw.ImageDraw, xy, text: str, font: ImageFont.FreeTypeFont, fill, tracking: int) -> int:
    """Рисует текст с межбуквенным интервалом. Возвращает занятую ширину."""
    x, y = xy
    for ch in text:
        draw.text((x, y), ch, font=font, fill=fill)
        x += int(draw.textlength(ch, font=font)) + tracking
    return x - xy[0]


def tracked_width(draw: ImageDraw.ImageDraw, text: str, font: ImageFont.FreeTypeFont, tracking: int) -> int:
    if not text:
        return 0
    return int(sum(draw.textlength(ch, font=font) for ch in text)) + tracking * (len(text) - 1)


def pipeline_schematic(rng: np.random.Generator) -> Image.Image:
    """Тонкая схема: узлы и связи цепочки.

    Не украшение: продукт — это цепочки плагинов, и схема говорит об этом
    прямо, одной линией, не превращаясь в диаграмму. Рисуется под словом и
    за ним, полупрозрачно, чтобы читаться как фактура, а не как рисунок.
    """
    layer = Image.new("RGBA", (W, H), (0, 0, 0, 0))
    d = ImageDraw.Draw(layer)

    cols = 9
    rows = 3
    pts = []
    for r in range(rows):
        for c in range(cols):
            jitter = rng.normal(0, 1, 2)
            x = W * 0.06 + (W * 0.88) * c / (cols - 1) + jitter[0] * 26
            y = H * 0.20 + (H * 0.60) * r / (rows - 1) + jitter[1] * 22
            pts.append((x, y, r, c))

    def node(x, y):
        d.rectangle([x - 5, y - 5, x + 5, y + 5], outline=ACCENT + (70,), width=2)

    def edge(a, b):
        d.line([a, b], fill=ACCENT + (42,), width=2)

    # Связи: вправо по строке, плюс диагонали между соседними строками.
    for i, (x, y, r, c) in enumerate(pts):
        node(x, y)
        if c + 1 < cols:
            edge((x, y), pts[i + 1][:2])
        if r + 1 < rows and (r + c) % 3 == 0:
            edge((x, y), pts[i + cols][:2])
    return layer.filter(ImageFilter.GaussianBlur(0.4))


def pick_font(candidates, size: int):
    for path in candidates:
        if os.path.exists(path):
            try:
                return ImageFont.truetype(path, size)
            except OSError:
                continue
    return ImageFont.load_default()


def main() -> None:
    rng = np.random.default_rng(20261020)
    img = background(rng)

    word_font = pick_font(
        [
            r"C:\Windows\Fonts\bahnschrift.ttf",
            r"C:\Windows\Fonts\consolab.ttf",
            r"C:\Windows\Fonts\calibrib.ttf",
        ],
        232,
    )
    mono_font = pick_font(
        [
            r"C:\Windows\Fonts\consola.ttf",
            r"C:\Windows\Fonts\cour.ttf",
        ],
        30,
    )

    word = "fidespipe"
    tracking = 18
    probe = ImageDraw.Draw(Image.new("RGB", (1, 1)))
    width = tracked_width(probe, word, word_font, tracking)
    bbox = probe.textbbox((0, 0), word, font=word_font)
    height = bbox[3] - bbox[1]

    # Схема цепочки — под словом, полупрозрачно.
    schematic = pipeline_schematic(rng)
    img.paste(schematic, (0, 0), schematic)

    # Тонкая тень под словом — буквы «стоят» на фоне, а не висят в нём.
    shadow = Image.new("L", (width + 80, height + 80), 0)
    sd = ImageDraw.Draw(shadow)
    draw_tracked(sd, (40 - bbox[0], 40 - bbox[1]), word, word_font, 255, tracking)
    shadow = shadow.filter(ImageFilter.GaussianBlur(9))
    dark = Image.new("RGB", shadow.size, (0, 0, 0))
    img.paste(dark, (int((W - width) / 2) - 40, int((H - height) / 2) - 40 + 10), shadow)

    # Слово.
    layer = Image.new("RGBA", (width + 80, height + 80), (0, 0, 0, 0))
    ld = ImageDraw.Draw(layer)
    draw_tracked(ld, (40 - bbox[0], 40 - bbox[1]), word, word_font, INK + (255,), tracking)
    img.paste(layer, (int((W - width) / 2) - 40, int((H - height) / 2) - 40), layer)

    # Акцентная линия под словом и подпись.
    line_y = int(H / 2 + height / 2) + 34
    line_w = int(width * 0.34)
    line = Image.new("RGBA", (line_w, 2), ACCENT + (150,))
    img.paste(line, (int((W - line_w) / 2), line_y), line)

    sub = "CONTRACT · TRUST · JOURNAL · HUMAN IN THE LOOP"
    sub_w = tracked_width(probe, sub, mono_font, 6)
    sub_layer = Image.new("RGBA", (sub_w + 40, 60), (0, 0, 0, 0))
    sld = ImageDraw.Draw(sub_layer)
    draw_tracked(sld, (20, 10), sub, mono_font, ACCENT + (170,), 6)
    img.paste(sub_layer, (int((W - sub_w) / 2) - 20, line_y + 22), sub_layer)

    out = os.path.normpath(OUT)
    img.save(out, optimize=True)
    print("баннер записан:", out, img.size, os.path.getsize(out), "байт")


if __name__ == "__main__":
    main()
