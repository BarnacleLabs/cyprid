"""Jev-compatible /v1/systemone server for Cygnet on Apple Silicon.

Cygnet (github.com/blockbrain-ai/cygnet-recipe) reads frozen Gemma-4-12B-it as a
decision model: it presents the options as letters, reads the model's own
probability for each letter at a single answer position, and applies one
calibration temperature. Its published shim does the readout through vLLM.

This worker runs Cygnet's own shim code unchanged (downloaded at a pinned commit
and checked by sha256): its system prompt, option lettering, prompt layout,
per-letter probability summing and temperature. Only the vLLM call is replaced,
by an in-process MLX forward pass that returns what vLLM would: the log
probabilities of the option-letter tokens at the answer position, restricted to
those tokens (as vLLM's structured choice does), top 20 only.

Differences from upstream, all outside its readout: every question in a request is
answered (upstream answers only one named "decision"), and answers are completed
to Jev's shape (score value and legend, a confidence).
"""
import argparse
import hashlib
import importlib.util
import json
import math
import os
import sys
import threading
import time
import urllib.request
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

UPSTREAM_URL = ("https://raw.githubusercontent.com/blockbrain-ai/cygnet-recipe/"
                "74f955548f8d557927e7d7c623cacff8b88a0758/shim/cygnet_shim.py")
UPSTREAM_SHA256 = "34816429ed14e0aaecb82b50f0a40e7b33055170f0698dbdc6cb5dedbb2b1ef3"
UPSTREAM_PATH = os.path.join(os.path.dirname(os.path.abspath(__file__)), "cygnet_upstream.py")

# Cygnet's documented launch; upstream reads it from the environment at import.
os.environ.setdefault("SHIM_TEMPERATURE", "3.4")


def fetch_upstream():
    with urllib.request.urlopen(UPSTREAM_URL, timeout=60) as r:
        data = r.read()
    got = hashlib.sha256(data).hexdigest()
    if got != UPSTREAM_SHA256:
        raise SystemExit(f"cygnet_shim.py sha256 {got} does not match the pinned {UPSTREAM_SHA256}")
    with open(UPSTREAM_PATH, "wb") as f:
        f.write(data)
    print(f"fetched Cygnet's shim ({UPSTREAM_SHA256[:12]})", flush=True)


def load_upstream():
    with open(UPSTREAM_PATH, "rb") as f:
        if hashlib.sha256(f.read()).hexdigest() != UPSTREAM_SHA256:
            raise SystemExit(f"{UPSTREAM_PATH} has changed; reinstall to fetch the pinned version")
    spec = importlib.util.spec_from_file_location("cygnet_shim", UPSTREAM_PATH)
    mod = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(mod)
    return mod


def confidence(probs):
    """1 - normalised entropy: an approximation of Jev's (unpublished) confidence."""
    if len(probs) < 2:
        return 1.0
    h = -sum(p * math.log(p) for p in probs if p > 0)
    return max(0.0, 1.0 - h / math.log(len(probs)))


class Reader:
    """Gemma-4 on MLX, answering Cygnet's call_vllm contract."""

    def __init__(self, repo, C, max_context):
        import mlx.core as mx
        from mlx_vlm import load

        self.mx, self.C, self.max_context = mx, C, max_context
        name, _, revision = repo.partition("@")
        self.model, processor = load(name, revision=revision or None)
        self.tok = getattr(processor, "tokenizer", processor)
        self.lock = threading.Lock()  # one forward pass at a time
        # Token ids that vLLM's choice constraint allows for each letter: every token
        # whose text is exactly that letter, including byte-fallback tokens (Gemma's
        # vocabulary has more than one, which Cygnet's readout sums).
        self.letter_ids = {}
        for L in C.LETTERS:
            ids = set()
            for tok in (L, "<0x%02X>" % ord(L)):
                i = self.tok.convert_tokens_to_ids(tok)
                if isinstance(i, int) and i >= 0 and i != self.tok.unk_token_id:
                    ids.add(i)
            if not ids:
                raise SystemExit(f"no token for letter {L!r} in the tokenizer")
            self.letter_ids[L] = sorted(ids)

    def prompt_ids(self, prompt_text):
        messages = [{"role": "system", "content": self.C.SYSTEM}, {"role": "user", "content": prompt_text}]
        text = self.tok.apply_chat_template(messages, add_generation_prompt=True, tokenize=False,
                                            **self.C.CHAT_TEMPLATE_KWARGS)
        # The template already starts with <bos>; vLLM tokenises chat prompts without adding another.
        return self.tok.encode(text, add_special_tokens=False)

    def call_vllm(self, prompt_text, allowed_letters):
        ids = self.prompt_ids(prompt_text)
        if len(ids) > self.max_context:
            raise self.C.Unprocessable(f"over the context limit: {len(ids)} > {self.max_context} tokens")
        mx = self.mx
        with self.lock:
            out = self.model(mx.array([ids]))
            logits = out.logits[0, -1].astype(mx.float32)
            cand = [(L, i) for L in allowed_letters for i in self.letter_ids[L]]
            vals = logits[mx.array([i for _, i in cand])]
            logprobs = (vals - mx.logsumexp(vals)).tolist()
        top = sorted(((L, lp) for (L, _), lp in zip(cand, logprobs)), key=lambda t: -t[1])[: self.C.TOP_LOGPROBS]
        return {
            "choices": [{"logprobs": {"content": [{"top_logprobs": [{"token": t, "logprob": lp} for t, lp in top]}]}}],
            "usage": {"prompt_tokens": len(ids), "completion_tokens": 1, "total_tokens": len(ids) + 1},
        }


def jev_answer(ans, q):
    """Complete Cygnet's answer to Jev's shape without changing its probabilities."""
    t = ans["type"]
    if t == "noul":
        return {"type": "noul", "noul": round(ans["noul"], 6)}
    probs = ans["probabilities"]
    out = {"type": t, "probabilities": {k: round(v, 6) for k, v in probs.items()},
           "confidence": round(confidence(list(probs.values())), 6)}
    if t == "choice":
        out["choice"] = ans["choice"]
    else:
        levels = q.get("criteria") or []
        out["score"] = round(sum(int(k) * v for k, v in probs.items()), 6)
        out["legend"] = {str(i): lvl if isinstance(lvl, str) else json.dumps(lvl) for i, lvl in enumerate(levels)}
    return out


def make_handler(C, name):
    class Handler(BaseHTTPRequestHandler):
        protocol_version = "HTTP/1.1"

        def send(self, status, obj):
            data = json.dumps(obj).encode()
            self.send_response(status)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(data)))
            self.end_headers()
            self.wfile.write(data)

        def do_GET(self):
            if self.path == "/health":
                self.send(200, {"status": "ok", "model": name, "temperature": C.TEMPERATURE})
            else:
                self.send(404, {"detail": "not found"})

        def do_POST(self):
            if self.path != "/v1/systemone":
                self.send(404, {"detail": "not found"})
                return
            try:
                body = json.loads(self.rfile.read(int(self.headers.get("Content-Length") or 0)) or b"null")
                questions = body.get("questions") if isinstance(body, dict) else None
                if not isinstance(questions, dict) or not questions:
                    raise ValueError("questions must be a non-empty object")
            except ValueError as e:
                self.send(422, {"detail": str(e)})
                return
            started, answers, tokens = time.perf_counter(), {}, 0
            try:
                for qid, q in questions.items():
                    if not isinstance(q, dict):
                        raise C.Unprocessable(f"questions.{qid} must be an object")
                    ans, usage, err = C.answer_for(body.get("state") or "", q)
                    if err:
                        self.send(502, {"detail": f"questions.{qid}: {err}"})
                        return
                    answers[qid] = jev_answer(ans, q)
                    tokens += usage.get("prompt_tokens") or 0
            except C.Unprocessable as e:
                self.send(422, {"detail": str(e)})
                return
            except Exception as e:
                self.send(500, {"detail": f"{type(e).__name__}: {e}"})
                return
            self.send(200, {"model": name, "answers": answers,
                            "usage": {"input_tokens": tokens, "output_tokens": len(answers)},
                            "latency_ms": round((time.perf_counter() - started) * 1000, 1)})

        def log_message(self, fmt, *args):
            sys.stderr.write("%s\n" % (fmt % args))

    return Handler


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--model", help="MLX Gemma-4 repo id, optionally @revision")
    ap.add_argument("--name", default="cygnet")
    ap.add_argument("--host", default="127.0.0.1")
    ap.add_argument("--port", type=int, default=8009)
    ap.add_argument("--max-context", type=int, default=16384, help="Cygnet's documented limit")
    ap.add_argument("--fetch-upstream", action="store_true", help="download Cygnet's pinned shim and exit")
    ap.add_argument("--prefetch", action="store_true", help="load the model once (downloading it) and exit")
    a = ap.parse_args()

    if a.fetch_upstream:
        fetch_upstream()
        return
    C = load_upstream()
    reader = Reader(a.model, C, a.max_context)
    C.call_vllm = reader.call_vllm  # the one replaced function; answer_for looks it up at call time
    print(f"loaded {a.model}; temperature {C.TEMPERATURE}", flush=True)
    if a.prefetch:
        return
    server = ThreadingHTTPServer((a.host, a.port), make_handler(C, a.name))
    print(f"serving {a.name} on http://{a.host}:{a.port}", flush=True)
    server.serve_forever()


if __name__ == "__main__":
    main()
