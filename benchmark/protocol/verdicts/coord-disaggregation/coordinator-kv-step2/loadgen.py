"""Step 2 load generator (stdlib only): Poisson arrivals through the gateway.

Each request = one of N shared prefixes (PREFIX_WORDS words) + a fresh suffix.
A miss suffix (MISS_WORDS) leaves more than the decider's nonCachedTokens uncached,
so it disaggregates through prefill; a hit suffix (HIT_WORDS) stays under it, so
decode serves it inline once the prefix is in its cache. Prints one CSV line per
request: t_send, kind, status, latency_s.
"""
import json
import os
import random
import sys
import threading
import time
import urllib.request

URL = os.environ.get("URL", "http://llm-d-inference-gateway-istio/v1/completions")
MODEL = os.environ.get("MODEL", "Qwen/Qwen3-VL-32B-Instruct")
RATE = float(os.environ.get("RATE", "0.5"))          # req/s
DURATION = float(os.environ.get("DURATION", "600"))  # s
P_MISS = float(os.environ.get("P_MISS", "0.5"))
PREFIXES = int(os.environ.get("PREFIXES", "4"))
PREFIX_WORDS = int(os.environ.get("PREFIX_WORDS", "2048"))
MISS_WORDS = int(os.environ.get("MISS_WORDS", "3072"))
HIT_WORDS = int(os.environ.get("HIT_WORDS", "512"))
MAX_TOKENS = int(os.environ.get("MAX_TOKENS", "128"))
SEED = int(os.environ.get("SEED", "1"))

WORDS = ("time year people way day man thing woman life child world school state family student "
         "group country problem hand part place case week company system program question work "
         "government number night point home water room mother area money story fact month lot "
         "right study book eye job word business issue side kind head house service friend father "
         "power hour game line end member law car city community name president team minute idea").split()

rng = random.Random(SEED)
prefixes = [" ".join(rng.choice(WORDS) for _ in range(PREFIX_WORDS)) for _ in range(PREFIXES)]
lock = threading.Lock()


def send(kind: str, prompt: str, t0: float) -> None:
    body = json.dumps({"model": MODEL, "prompt": prompt, "max_tokens": MAX_TOKENS, "temperature": 0}).encode()
    req = urllib.request.Request(URL, data=body, headers={"Content-Type": "application/json"})
    start = time.time()
    try:
        with urllib.request.urlopen(req, timeout=600) as r:
            r.read(); status = r.status
    except urllib.error.HTTPError as e:
        status = e.code
    except Exception as e:  # noqa: BLE001
        status = f"err:{type(e).__name__}"
    with lock:
        print(f"{start - t0:.2f},{kind},{status},{time.time() - start:.2f}", flush=True)


def main() -> None:
    print("t_send,kind,status,latency_s", flush=True)
    t0 = time.time(); threads = []
    # warm each prefix once so later hits find it in a decode cache
    for p in prefixes:
        threads.append(threading.Thread(target=send, args=("warm", p + " " + " ".join(rng.choice(WORDS) for _ in range(HIT_WORDS)), t0)))
        threads[-1].start(); time.sleep(1)
    while time.time() - t0 < DURATION:
        time.sleep(rng.expovariate(RATE))
        kind = "miss" if rng.random() < P_MISS else "hit"
        suffix = " ".join(rng.choice(WORDS) for _ in range(MISS_WORDS if kind == "miss" else HIT_WORDS))
        th = threading.Thread(target=send, args=(kind, rng.choice(prefixes) + " " + suffix, t0), daemon=True)
        th.start(); threads.append(th)
    for th in threads:
        th.join(timeout=650)


if __name__ == "__main__":
    sys.exit(main())
