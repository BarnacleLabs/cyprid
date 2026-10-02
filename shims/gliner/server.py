"""Jev-compatible /v1/systemone server for GLiNER2 classification models.

Written into the model's environment by cyprid and launched as its worker.
GLiNER2 only classifies, so each Jev question becomes one single-label task:

  choice  native: the criteria keys are the labels
  noul    emulated: a yes/no task, noul = P(yes)
  score   emulated: one ordered label per level, score = the probability-weighted level index

Criteria descriptions (choice option texts, noul criteria.true/false) are passed
to GLiNER as label descriptions. On the kev-transfer-v4 bench that lifts
accuracy from 45.6% to 54.3% (SciQ 32% -> 85%, where option keys are just a-d);
--no-label-descriptions turns it off.

Emulated answers carry "x_emulated": true. Confidence is 1 - normalised entropy
of the distribution, an approximation of Jev's (unpublished) formula.
"""
import argparse
import json
import math
import sys
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

MAX_CHOICE_OPTIONS = 255
MAX_SCORE_LEVELS = 10
# GLiNER2 injects these markers into its prompt and rejects labels containing them.
RESERVED = ("[P]", "[L]", "[C]", "[E]", "[R]", "[DESCRIPTION]", "[EXAMPLE]", "[OUTPUT]")


class BadRequest(Exception):
    pass


def render(v, indent=0):
    """Flatten Jev's string | object | array content into text, keeping field names as labels."""
    pad = "  " * indent
    if v is None:
        return ""
    if isinstance(v, (str, int, float, bool)):
        return str(v)
    if isinstance(v, list):
        return "\n".join(f"{pad}- {render(x, indent + 1).lstrip()}" for x in v)
    return "\n".join(
        f"{pad}{k}:\n{render(x, indent + 1)}" if isinstance(x, (dict, list)) else f"{pad}{k}: {render(x)}"
        for k, x in v.items()
    )


def clean(text):
    """Make text safe for GLiNER2's prompt: drop marker tokens, swap parentheses, collapse whitespace."""
    text = text.replace("(", "[").replace(")", "]")  # before stripping, so "(P)" can't become "[P]"
    for token in RESERVED:
        text = text.replace(token, " ")
    return " ".join(text.split())


def unique_labels(names):
    """Clean label names, keeping them distinct; returns [(clean_name, original_key)]."""
    seen, out = set(), []
    for original in names:
        name = clean(str(original)) or "option"
        base, n = name, 2
        while name in seen:
            name, n = f"{base} {n}", n + 1
        seen.add(name)
        out.append((name, original))
    return out


def confidence(probs):
    if len(probs) < 2:
        return 1.0
    entropy = -sum(p * math.log(p) for p in probs if p > 0)
    return max(0.0, 1.0 - entropy / math.log(len(probs)))


class Decider:
    def __init__(self, repo, device, label_descriptions=True):
        from gliner2.classification import Classifier, ClassificationSchema

        self.Schema = ClassificationSchema
        self.clf = Classifier.from_pretrained(repo, device=device)
        self.clf.eval()
        self.lock = threading.Lock()  # one forward pass at a time
        self.label_descriptions = label_descriptions
        processor = getattr(self.clf.model, "processor", None)
        self.tokenizer = getattr(processor, "tokenizer", None)

    def distribution(self, text, instruction, labels, ordered=False):
        """labels: [(name, description or None)] -> [probability] in the same order."""
        schema = self.Schema().single(
            "decision",
            {name: (desc or None) if self.label_descriptions else None for name, desc in labels},
            instruction=instruction or None,
            ordered=ordered,
        )
        with self.lock:
            scores = self.clf.score(text, schema)
        probs = [scores.probability("decision", name) for name, _ in labels]
        total = sum(probs) or 1.0
        return [p / total for p in probs]

    def count_tokens(self, text):
        try:
            return len(self.tokenizer.encode(text))
        except Exception:
            return len(text.split())

    def answer(self, text, q, qid):
        if not isinstance(q, dict):
            raise BadRequest(f"questions.{qid} must be an object")
        qtype = q.get("type")
        instruction = clean(render(q.get("instructions")))
        if not instruction:
            raise BadRequest(f"questions.{qid}.instructions is required")
        criteria = q.get("criteria")

        if qtype == "noul":
            criteria = criteria or {}
            if not isinstance(criteria, dict):
                raise BadRequest(f"questions.{qid}.criteria must be an object with true/false descriptions")
            labels = [("yes", clean(render(criteria.get("true")))), ("no", clean(render(criteria.get("false"))))]
            p_yes, _ = self.distribution(text, instruction, labels)
            return {"type": "noul", "noul": round(p_yes, 6), "x_emulated": True}

        if qtype == "choice":
            if not isinstance(criteria, dict) or not criteria:
                raise BadRequest(f"questions.{qid}.criteria must map each option to a description (or null)")
            if len(criteria) > MAX_CHOICE_OPTIONS:
                raise BadRequest(f"questions.{qid}: at most {MAX_CHOICE_OPTIONS} options")
            names = unique_labels(criteria.keys())
            labels = [(name, clean(render(criteria[key]))) for name, key in names]
            probs = self.distribution(text, instruction, labels)
            by_key = {key: round(p, 6) for (_, key), p in zip(names, probs)}
            return {
                "type": "choice",
                "choice": max(by_key, key=by_key.get),
                "probabilities": by_key,
                "confidence": round(confidence(probs), 6),
            }

        if qtype == "score":
            if not isinstance(criteria, list) or len(criteria) < 2:
                raise BadRequest(f"questions.{qid}.criteria must be an array of at least 2 levels")
            if len(criteria) > MAX_SCORE_LEVELS:
                raise BadRequest(f"questions.{qid}: at most {MAX_SCORE_LEVELS} levels")
            texts = [render(level) for level in criteria]
            names = unique_labels(texts)
            probs = self.distribution(text, instruction, [(name, None) for name, _ in names], ordered=True)
            return {
                "type": "score",
                "score": round(sum(i * p for i, p in enumerate(probs)), 6),
                "legend": {str(i): t for i, t in enumerate(texts)},
                "probabilities": {str(i): round(p, 6) for i, p in enumerate(probs)},
                "confidence": round(confidence(probs), 6),
                "x_emulated": True,
            }

        raise BadRequest(f"questions.{qid}.type must be noul, choice or score")

    def systemone(self, body, model_name):
        if not isinstance(body, dict):
            raise BadRequest("request body must be a JSON object")
        if "state" not in body:
            raise BadRequest("state is required")
        questions = body.get("questions")
        if not isinstance(questions, dict) or not questions:
            raise BadRequest("questions must be a non-empty object")
        text = render(body["state"])
        answers = {qid: self.answer(text, q, qid) for qid, q in questions.items()}
        return {
            "model": model_name,
            "answers": answers,
            "usage": {"input_tokens": self.count_tokens(text) * len(questions), "output_tokens": 0},
        }


def make_handler(decider, model_name):
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
                started = time.perf_counter()
                result = decider.systemone(body, model_name)
                result["latency_ms"] = round((time.perf_counter() - started) * 1000, 1)
                self.send(200, result)
            except (BadRequest, json.JSONDecodeError) as e:
                self.send(422, {"detail": str(e)})
            except Exception as e:  # SchemaError and anything unexpected
                name = type(e).__name__
                self.send(422 if name == "SchemaError" else 500, {"detail": f"{name}: {e}"})

        def log_message(self, fmt, *args):
            sys.stderr.write("%s\n" % (fmt % args))

    return Handler


def pick_device(requested):
    if requested != "auto":
        return requested
    import torch

    return "mps" if torch.backends.mps.is_available() else "cpu"


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--repo", required=True, help="Hugging Face repo id or local path")
    ap.add_argument("--name", default="gliner2")
    ap.add_argument("--host", default="127.0.0.1")
    ap.add_argument("--port", type=int, default=8000)
    # gliner2 2.0.0 mixes CPU and MPS tensors in its classification scorer, so default to CPU
    # (fast enough for a 340M encoder); pass --device mps to try the GPU.
    ap.add_argument("--device", default="cpu", help="cpu, mps or auto")
    ap.add_argument("--no-label-descriptions", dest="label_descriptions", action="store_false",
                    help="don't pass criteria descriptions to the model")
    ap.add_argument("--prefetch", action="store_true", help="download the weights, check they load, and exit")
    a = ap.parse_args()

    device = pick_device(a.device)
    print(f"loading {a.repo} on {device}", flush=True)
    decider = Decider(a.repo, device, a.label_descriptions)
    if a.prefetch:
        print("weights downloaded and loaded OK", flush=True)
        return

    server = ThreadingHTTPServer((a.host, a.port), make_handler(decider, a.name))
    print(f"serving {a.name} on http://{a.host}:{a.port}", flush=True)
    server.serve_forever()


if __name__ == "__main__":
    main()
