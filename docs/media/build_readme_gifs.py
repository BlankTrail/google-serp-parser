#!/usr/bin/env python3
"""Build bilingual README GIF tours from the committed interface screenshots.

Run: python docs/media/build_readme_gifs.py [--language ru|en] [--tour ...]
Requires Pillow. These are annotated screenshot tours, not live recordings.
Only scaling and viewport scrolling are applied to the screenshot pixels.
"""

import argparse
from functools import lru_cache
import math
import os
from pathlib import Path

from PIL import Image, ImageDraw, ImageFont


ROOT = Path(__file__).resolve().parents[2]
SOURCE = ROOT / "assets" / "screenshots"
OUTPUT = ROOT / "docs" / "img"
WIDTH, HEIGHT = 1200, 850
VIEW = (24, 144, 1176, 792)
BG, INK, PURPLE = "#F3F0FA", "#241A38", "#8B2CF5"
TOURS = {
    "overview": {
        "ru": (
            "ЗАДАНИЕ В РАБОТЕ",
            [
                ("status", "Весь прогон — на одном экране",
                 "Прогресс, скорость, ошибки и очередь заданий.", "Состояние"),
                ("job", "От статистики сессий до результатов",
                 "Прокрутка: пул, проверки Google, параметры и собранные строки.", "Задание"),
            ],
        ),
        "en": (
            "A JOB IN PROGRESS",
            [
                ("status", "See the whole run at a glance",
                 "Progress, speed, errors and the job queue.", "Status"),
                ("job", "From session statistics to results",
                 "Scroll through the pool, Google challenges, settings and captured rows.", "Job"),
            ],
        ),
    },
    "jobs": {
        "ru": (
            "СОЗДАНИЕ ЗАДАНИЯ",
            [
                ("new-job", "Выберите, что и как собирать",
                 "Тип задания, поля, глубина, профиль, потоки и исходные фразы.", "Параметры"),
                ("new-job-format", "Разверните фразы с помощью макросов",
                 "Несколько форматов запроса, буквенные и числовые подстановки.", "Макросы"),
                ("new-job-suggest", "Соберите поисковые подсказки",
                 "Отдельный режим: язык, Multiword и лимит запросов на ключ.", "Подсказки"),
            ],
        ),
        "en": (
            "CREATE A JOB",
            [
                ("new-job", "Choose what to collect and how",
                 "Job type, fields, depth, profile, threads and seed keywords.", "Settings"),
                ("new-job-format", "Expand keywords with macros",
                 "Multiple query formats, alphabet substitutions and number ranges.", "Macros"),
                ("new-job-suggest", "Collect search suggestions",
                 "A dedicated mode: language, Multiword and requests per keyword.", "Suggestions"),
            ],
        ),
    },
    "proxies": {
        "ru": (
            "ПРОФИЛИ ПРОКСИ И VPN",
            [
                ("proxies", "Разделите наборы выходов по профилям",
                 "Профиль выбирается в задании; основной используется по умолчанию.", "Профили"),
                ("proxies-gateways", "Выберите VPN-шлюзы из BlankTrail",
                 "Настройки пула, подписки, отдельные конфигурации и отклик шлюзов.", "VPN"),
            ],
        ),
        "en": (
            "PROXY AND VPN PROFILES",
            [
                ("proxies", "Keep exit pools in separate profiles",
                 "Choose a profile per job, or use the default profile.", "Profiles"),
                ("proxies-gateways", "Select BlankTrail VPN gateways",
                 "Pool settings, subscriptions, individual configurations and latency.", "VPN"),
            ],
        ),
    },
}


@lru_cache(maxsize=None)
def font(size, bold=False):
    names = ["segoeuib.ttf", "DejaVuSans-Bold.ttf"] if bold else ["segoeui.ttf", "DejaVuSans.ttf"]
    roots = [Path(os.environ.get("WINDIR", "C:/Windows")) / "Fonts",
             Path("/usr/share/fonts/truetype/dejavu")]
    for folder in roots:
        for name in names:
            path = folder / name
            if path.is_file():
                return ImageFont.truetype(str(path), size)
    raise RuntimeError("Segoe UI or DejaVu Sans with Cyrillic support is required")


def draw_fitted(draw, xy, text, size, fill, bold=False, max_width=WIDTH - 64):
    """Preserve captions in both languages without clipping at the right edge."""
    face = font(size, bold)
    while draw.textlength(text, font=face) > max_width and size > 14:
        size -= 1
        face = font(size, bold)
    if draw.textlength(text, font=face) > max_width:
        raise ValueError(f"Caption does not fit: {text}")
    draw.text(xy, text, font=face, fill=fill)


def shell(eyebrow, steps, current, language):
    canvas = Image.new("RGB", (WIDTH, HEIGHT), BG)
    draw = ImageDraw.Draw(canvas)
    draw.rectangle((0, 0, WIDTH, 128), fill=INK)
    draw.rectangle((0, 0, 7, 128), fill=PURPLE)
    draw.text((32, 15), "BlankTrail / Google Parser", font=font(17, True), fill="#DCC2FF")
    label = font(14, True)
    draw.text((WIDTH - 32 - draw.textlength(eyebrow, font=label), 18),
              eyebrow, font=label, fill="#DCC2FF")
    draw_fitted(draw, (32, 48), steps[current][1], 30, "white", True)
    draw_fitted(draw, (33, 94), steps[current][2], 18, "#E5DAF4")
    draw.rectangle((VIEW[0] - 1, VIEW[1] - 1, VIEW[2], VIEW[3]), fill="#DFD5EA")

    width = 760 / len(steps)
    for i, step in enumerate(steps):
        x = round(32 + i * width)
        draw.rounded_rectangle((x, 812, x + 22, 834), radius=11,
                               fill=PURPLE if i == current else "#DFD5EA")
        draw.text((x + 7, 813), str(i + 1), font=font(12, True),
                  fill="white" if i == current else "#6F647D")
        draw.text((x + 31, 812), step[3], font=font(15, i == current),
                  fill=INK if i == current else "#6F647D")
    note = "Обзор снимков интерфейса" if language == "ru" else "Interface screenshot tour"
    face = font(13)
    draw.text((WIDTH - 32 - draw.textlength(note, font=face), 814),
              note, font=face, fill="#6F647D")
    return canvas


def build(tour, language):
    eyebrow, steps = TOURS[tour][language]
    scenes = []
    for i, (source, _, _, _) in enumerate(steps):
        with Image.open(SOURCE / f"{source}-{language}.png") as original:
            shot = original.convert("RGB")
        width = VIEW[2] - VIEW[0]
        shot = shot.resize((width, round(shot.height * width / shot.width)), Image.Resampling.LANCZOS)
        scenes.append((shell(eyebrow, steps, i, language), shot))

    # One palette for the whole tour avoids frame-to-frame colour flicker.
    swatches = Image.new("RGB", (600, 400 * len(scenes)), BG)
    for i, (base, shot) in enumerate(scenes):
        swatches.paste(base.resize((600, 170)), (0, 400 * i))
        swatches.paste(shot.resize((600, 230)), (0, 400 * i + 170))
    palette = swatches.quantize(colors=256, method=Image.Quantize.MEDIANCUT)
    frames, durations = [], []
    viewport_height = VIEW[3] - VIEW[1]
    for base, shot in scenes:
        travel = max(0, shot.height - viewport_height)
        # Small overflows fit as a whole; tall forms keep their readable width.
        if 0 < travel < 140:
            shot.thumbnail((VIEW[2] - VIEW[0], viewport_height), Image.Resampling.LANCZOS)
            travel = 0
        count = max(25, math.ceil(travel / 24)) if travel else 1
        for index in range(count):
            t = index / (count - 1) if count > 1 else 0
            eased = t * t * (3 - 2 * t)
            offset = round(travel * eased)
            frame = base.copy()
            view = Image.new("RGB", (VIEW[2] - VIEW[0], viewport_height), "#F4F4F6")
            visible = shot.crop((0, offset, shot.width, min(shot.height, offset + viewport_height)))
            view.paste(visible, ((view.width - visible.width) // 2,
                                 (view.height - visible.height) // 2 if not travel else 0))
            frame.paste(view, (VIEW[0], VIEW[1]))
            draw = ImageDraw.Draw(frame)
            draw.rectangle((0, 124, WIDTH, 127), fill="#4D365F")
            draw.rectangle((0, 124, round(WIDTH * t), 127), fill="#B875FF")
            frames.append(frame.quantize(palette=palette, dither=Image.Dither.NONE))
            durations.append(5000 if count == 1 else 1800 if index in (0, count - 1) else 160)
    OUTPUT.mkdir(parents=True, exist_ok=True)
    target = OUTPUT / f"google-parser-{tour}-{language}.gif"
    frames[0].save(target, save_all=True, append_images=frames[1:], duration=durations,
                   loop=0, optimize=True, disposal=1,
                   comment=b"Annotated screenshot tour, not a live recording. Source: assets/screenshots/")
    print(f"{target.name}: {len(frames)} frames, {sum(durations)/1000:.1f}s, "
          f"{target.stat().st_size/1024:.0f} KiB", flush=True)


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--tour", choices=list(TOURS))
    parser.add_argument("--language", choices=["ru", "en"])
    args = parser.parse_args()
    for tour in TOURS:
        for language in ("ru", "en"):
            if (not args.tour or args.tour == tour) and (not args.language or args.language == language):
                build(tour, language)
