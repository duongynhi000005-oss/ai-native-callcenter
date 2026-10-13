#!/usr/bin/env python3
# SPDX-License-Identifier: Apache-2.0
"""Render the language-drift test set (issue #127) with macOS `say` and ffmpeg.

For every utterance in scripts_<lang>.tsv and each of three variants:

  clean      the utterance alone
  noise      plus seeded low-pass background noise, 25 dB below the speech
  crosstalk  plus a second speaker (another voice, another line of the same
             language) 15 dB below the speech, the room of #26

the signal is band-limited to 8 kHz, passed through G.711 mu-law
(encode + decode) and resampled to 16 kHz PCM16 mono, which is what the tap
hands the transcription client. Output goes to cache/<lang>/<variant>/<id>.pcm
(gitignored). The run is deterministic: fixed voices, fixed seeds, no clock.

Usage: generate.py [--out DIR] [--only ID,...]
Requires macOS `say` with the Tingting, Eddy (Chinese (China mainland)),
Samantha and Daniel voices, and ffmpeg.
"""
import argparse
import array
import json
import math
import os
import random
import subprocess
import sys
import tempfile
import zlib

HERE = os.path.dirname(os.path.abspath(__file__))
VOICES = {
    "zh": {"main": "Tingting", "other": "Eddy (Chinese (China mainland))"},
    "en": {"main": "Samantha", "other": "Daniel"},
}
NOISE_DB = -25.0
CROSSTALK_DB = -15.0
CROSSTALK_SHIFT = 7  # the second speaker reads the line seven further on


def run(cmd, data=None):
    p = subprocess.run(cmd, input=data, stdout=subprocess.PIPE, stderr=subprocess.PIPE, check=False)
    if p.returncode != 0:
        sys.exit("%s failed: %s" % (cmd[0], p.stderr.decode(errors="replace")[-400:]))
    return p.stdout


def tts8k(voice, text, tmp):
    """Speak text and return mono PCM16 samples at 8 kHz."""
    aiff = os.path.join(tmp, "t.aiff")
    run(["say", "-v", voice, "-o", aiff, text])
    raw = run(["ffmpeg", "-v", "error", "-i", aiff, "-ac", "1", "-ar", "8000", "-f", "s16le", "-"])
    a = array.array("h")
    a.frombytes(raw)
    return a


def rms(a):
    return math.sqrt(sum(x * x for x in a) / max(len(a), 1)) or 1.0


def clip(v):
    return max(-32768, min(32767, int(round(v))))


def add_noise(speech, seed):
    rng = random.Random(seed)
    gain = rms(speech) * 10 ** (NOISE_DB / 20)
    out = array.array("h")
    lp = 0.0
    n = []
    for _ in speech:
        lp = 0.9 * lp + rng.gauss(0, 1)  # one-pole low pass: hum, not hiss
        n.append(lp)
    scale = gain / (math.sqrt(sum(x * x for x in n) / len(n)) or 1.0)
    for s, x in zip(speech, n):
        out.append(clip(s + x * scale))
    return out


def add_crosstalk(speech, other):
    gain = rms(speech) * 10 ** (CROSSTALK_DB / 20) / rms(other)
    out = array.array("h")
    for i, s in enumerate(speech):
        o = other[i] if i < len(other) else 0
        out.append(clip(s + o * gain))
    return out


def channel16k(a):
    """G.711 mu-law round trip at 8 kHz, then 16 kHz PCM16 mono."""
    enc = run(["ffmpeg", "-v", "error", "-f", "s16le", "-ar", "8000", "-ac", "1", "-i", "-",
               "-c:a", "pcm_mulaw", "-f", "mulaw", "-"], a.tobytes())
    return run(["ffmpeg", "-v", "error", "-f", "mulaw", "-ar", "8000", "-ac", "1", "-i", "-",
                "-ar", "16000", "-f", "s16le", "-"], enc)


def load(lang):
    rows = []
    with open(os.path.join(HERE, "scripts_%s.tsv" % lang), encoding="utf-8") as f:
        for line in f:
            line = line.rstrip("\n")
            if line:
                rows.append(line.split("\t"))
    return rows


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--out", default=os.path.join(HERE, "cache"))
    ap.add_argument("--only", default="")
    args = ap.parse_args()
    only = set(filter(None, args.only.split(",")))
    manifest = []
    with tempfile.TemporaryDirectory() as tmp:
        for lang in ("zh", "en"):
            rows = load(lang)
            for i, (uid, cat, say, ref) in enumerate(rows):
                if only and uid not in only:
                    continue
                speech = tts8k(VOICES[lang]["main"], say, tmp)
                other_row = rows[(i + CROSSTALK_SHIFT) % len(rows)]
                other = tts8k(VOICES[lang]["other"], other_row[2], tmp)
                variants = {
                    "clean": speech,
                    "noise": add_noise(speech, zlib.crc32(uid.encode())),
                    "crosstalk": add_crosstalk(speech, other),
                }
                for name, sig in variants.items():
                    d = os.path.join(args.out, lang, name)
                    os.makedirs(d, exist_ok=True)
                    pcm = channel16k(sig)
                    with open(os.path.join(d, uid + ".pcm"), "wb") as f:
                        f.write(pcm)
                manifest.append({"id": uid, "lang": lang, "category": cat,
                                 "seconds": round(len(speech) / 8000, 2)})
                print(uid, round(len(speech) / 8000, 1), "s", flush=True)
    with open(os.path.join(args.out, "manifest.json"), "w", encoding="utf-8") as f:
        json.dump(manifest, f, ensure_ascii=False, indent=1)


if __name__ == "__main__":
    main()
