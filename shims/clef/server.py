"""Jev-compatible /v1/systemone server for Cloudflare's Clef models.

Written into the model's environment by cyprid and launched as its worker.
Clef already answers Jev requests: its weights folder ships joint_schema_model.py,
whose systemone() takes a /v1/systemone request body and returns the response
body. This worker loads the model onto the Mac's GPU (MPS) and serves that
function over HTTP. The code comes from the pinned weights revision, so it
changes only when the recipe's revision does.

Clef reads the whole input in one forward pass and scores every option of every
question jointly; there's no text generation, so answers are deterministic.
"""
import argparse
import json
import os
import sys
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer


def load(path, device):
    import torch

    sys.path.insert(0, path)
    from joint_schema_model import load_release_model, systemone

    model, processor = load_release_model(path, device=device, dtype=torch.bfloat16)
    return model, processor, systemone


class OutOfMemory(Exception):
    pass


def release_memory():
    """Hand PyTorch's cached GPU memory back to macOS after each request."""
    import gc

    import torch

    gc.collect()
    if torch.backends.mps.is_available():
        torch.mps.empty_cache()


def answer(run):
    """Run one request, releasing memory afterwards and retrying once if the GPU runs out.

    On a Mac where the model only just fits (Clef-flash on 36GB), long inputs
    leave memory held inside the GPU driver that freeing PyTorch's cache doesn't
    return; once it fills, every request fails. Only a new process clears it, so
    a second failure raises OutOfMemory and the worker exits after replying, and
    cyprid starts a fresh one on the next request.
    """
    try:
        return run()
    except RuntimeError as e:
        if "out of memory" not in str(e):
            raise
        release_memory()
        try:
            return run()
        except RuntimeError as e:
            if "out of memory" in str(e):
                raise OutOfMemory(str(e)) from e
            raise
    finally:
        release_memory()


def make_handler(model, processor, systemone, model_name, max_length):
    lock = threading.Lock()  # one forward pass at a time: the GPU is shared

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
                self.send(200, {"status": "ok", "model": model_name})
            else:
                self.send(404, {"detail": "not found"})

        def do_POST(self):
            if self.path != "/v1/systemone":
                self.send(404, {"detail": "not found"})
                return
            try:
                length = int(self.headers.get("Content-Length") or 0)
                body = json.loads(self.rfile.read(length) or b"null")
                if not isinstance(body, dict):
                    raise ValueError("request body must be a JSON object")
                started = time.perf_counter()
                with lock:
                    result = answer(lambda: systemone(model, processor, {**body, "model": model_name}, max_length=max_length))
                result["latency_ms"] = round((time.perf_counter() - started) * 1000, 1)
                self.send(200, result)
            except OutOfMemory as e:
                self.send(503, {"detail": f"out of GPU memory; restarting the worker (try again): {e}"})
                sys.stderr.write("out of GPU memory: exiting so cyprid restarts the worker\n")
                os._exit(75)
            except (ValueError, KeyError, json.JSONDecodeError) as e:
                self.send(422, {"detail": f"{type(e).__name__}: {e}"})
            except Exception as e:
                self.send(500, {"detail": f"{type(e).__name__}: {e}"})

        def log_message(self, fmt, *args):
            sys.stderr.write("%s\n" % (fmt % args))

    return Handler


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--path", required=True, help="the Clef weights folder (with joint_schema_model.py)")
    ap.add_argument("--name", default="clef")
    ap.add_argument("--host", default="127.0.0.1")
    ap.add_argument("--port", type=int, default=8000)
    ap.add_argument("--device", default="mps", help="mps or cpu")
    ap.add_argument("--max-length", type=int, default=16384,
                    help="longest input in tokens, questions included (Clef's default)")
    ap.add_argument("--prefetch", action="store_true", help="check the weights load, and exit")
    a = ap.parse_args()

    print(f"loading {a.path} on {a.device}", flush=True)
    started = time.perf_counter()
    model, processor, systemone = load(a.path, a.device)
    print(f"loaded in {time.perf_counter() - started:.0f}s", flush=True)
    if a.prefetch:
        print("weights loaded OK", flush=True)
        return

    handler = make_handler(model, processor, systemone, a.name, a.max_length)
    server = ThreadingHTTPServer((a.host, a.port), handler)
    print(f"serving {a.name} on http://{a.host}:{a.port}", flush=True)
    server.serve_forever()


if __name__ == "__main__":
    main()
