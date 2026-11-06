#!/usr/bin/env python3
"""OpenAI-Batches shim backed by OpenRouter, for delphi's openai provider.

delphi's openai path speaks the OpenAI Files + Batches API and puts a
`/v1/responses` request in each batch line.  OpenRouter speaks chat
completions and has no batch API, so this fronts it:

    POST /v1/files              upload the JSONL
    POST /v1/batches            create a batch, start the worker
    GET  /v1/batches/{id}       status, then output_file_id
    GET  /v1/files/{id}/content the output JSONL

Set OPENROUTER_API_KEY (or put it in ~/.delphi/openrouter.env).  With no key
the shim answers "shim-ok" so the wiring can be tested without spend.
"""
import json, os, threading, time, uuid
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from urllib import request as urlrequest

PORT = int(os.environ.get("SHIM_PORT", "8788"))
STORE = os.path.expanduser("~/.delphi/shim")
BASE = "https://openrouter.ai/api/v1"
KEY = os.environ.get("OPENROUTER_API_KEY", "").strip()
if not KEY:
    env = os.path.expanduser("~/.delphi/openrouter.env")
    if os.path.exists(env):
        for line in open(env):
            line = line.strip()
            if line.startswith("OPENROUTER_API_KEY="):
                KEY = line.split("=", 1)[1].strip().strip('"')
DEFAULT_MODEL = os.environ.get("OPENROUTER_MODEL", "nvidia/nemotron-3-ultra-550b-a55b:free")

os.makedirs(STORE, exist_ok=True)
FILES, BATCHES = {}, {}


def responses_object(mid, model, text, usage=None):
    u = usage or {}
    return {
        "id": "resp_" + mid, "object": "response",
        "created_at": int(time.time()), "status": "completed", "model": model,
        "output": [{
            "type": "message", "id": "msg_" + mid, "status": "completed",
            "role": "assistant",
            "content": [{"type": "output_text", "text": text, "annotations": []}],
        }],
        "output_text": text,
        "usage": {"input_tokens": u.get("prompt_tokens", 0),
                  "output_tokens": u.get("completion_tokens", 0),
                  "total_tokens": u.get("total_tokens", 0)},
    }


def call_openrouter(model, body):
    messages = []
    if isinstance(body.get("instructions"), str):
        messages.append({"role": "system", "content": body["instructions"]})
    for item in body.get("input", []):
        if isinstance(item, dict) and item.get("role"):
            c = item.get("content")
            messages.append({"role": item["role"],
                             "content": c if isinstance(c, str) else json.dumps(c)})
    payload = {"model": model or DEFAULT_MODEL, "messages": messages,
               "max_tokens": body.get("max_output_tokens") or 4096}
    req = urlrequest.Request(BASE + "/chat/completions",
                             data=json.dumps(payload).encode(),
                             headers={"content-type": "application/json",
                                      "authorization": "Bearer " + KEY})
    with urlrequest.urlopen(req, timeout=300) as r:
        d = json.load(r)
    return (d["choices"][0]["message"].get("content") or "", d.get("usage") or {}, d.get("model", model))


def run_batch(bid):
    b = BATCHES[bid]
    b["status"] = "in_progress"
    out, errs, ok, bad = [], [], 0, 0
    for line in open(b["input_path"]):
        line = line.strip()
        if not line:
            continue
        rec = json.loads(line)
        cid = rec.get("custom_id")
        body = rec.get("body") or {}
        model = body.get("model") or DEFAULT_MODEL
        try:
            if KEY:
                text, usage, served = call_openrouter(model, body)
            else:
                text, usage, served = "shim-ok", {}, model
            ok += 1
            out.append({"id": "batch_req_" + uuid.uuid4().hex[:12], "custom_id": cid,
                        "response": {"status_code": 200, "request_id": "shim",
                                     "body": responses_object(uuid.uuid4().hex[:8], served, text, usage)},
                        "error": None})
        except Exception as e:
            bad += 1
            errs.append({"id": "batch_req_" + uuid.uuid4().hex[:12], "custom_id": cid,
                         "response": {"status_code": 500, "body": None},
                         "error": {"code": "shim_error", "message": str(e)[:300]}})
    fid = "file-out-" + uuid.uuid4().hex[:12]
    path = os.path.join(STORE, fid + ".jsonl")
    with open(path, "w") as f:
        for r in out + errs:
            f.write(json.dumps(r) + "\n")
    FILES[fid] = path
    b.update(status="completed", output_file_id=fid, counts={"total": ok + bad, "completed": ok, "failed": bad})


class H(BaseHTTPRequestHandler):
    def _json(self, code, obj):
        data = json.dumps(obj).encode()
        self.send_response(code)
        self.send_header("content-type", "application/json")
        self.send_header("content-length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)

    def do_POST(self):
        n = int(self.headers.get("content-length") or 0)
        raw = self.rfile.read(n)
        if self.path.endswith("/files"):
            fid = "file-in-" + uuid.uuid4().hex[:12]
            path = os.path.join(STORE, fid + ".jsonl")
            ctype = self.headers.get("content-type", "")
            boundary = ctype.split("boundary=")[-1].encode()
            body = raw.split(b"--" + boundary)
            for part in body:
                if b'name="file"' in part:
                    payload = part.split(b"\r\n\r\n", 1)[1].rsplit(b"\r\n", 1)[0]
                    open(path, "wb").write(payload)
            FILES[fid] = path
            return self._json(200, {"id": fid, "object": "file", "bytes": os.path.getsize(path),
                                    "created_at": int(time.time()), "filename": fid + ".jsonl",
                                    "purpose": "batch", "status": "processed"})
        if self.path.endswith("/batches"):
            d = json.loads(raw or b"{}")
            src = d.get("input_file_id")
            bid = "batch_" + uuid.uuid4().hex[:16]
            BATCHES[bid] = {"status": "validating", "input_path": FILES.get(src),
                            "input_file_id": src, "counts": {"total": 0, "completed": 0, "failed": 0}}
            threading.Thread(target=run_batch, args=(bid,), daemon=True).start()
            return self._json(200, {"id": bid, "object": "batch", "status": "validating",
                                    "input_file_id": src, "completion_window": "24h",
                                    "created_at": int(time.time()), "endpoint": "/v1/responses",
                                    "request_counts": BATCHES[bid]["counts"]})
        self._json(404, {"error": {"message": "no route " + self.path}})

    def do_GET(self):
        p = self.path.split("?")[0]
        if p.startswith("/v1/batches/"):
            b = BATCHES.get(p.rsplit("/", 1)[-1])
            if not b:
                return self._json(404, {"error": {"message": "no batch"}})
            out = {"id": p.rsplit("/", 1)[-1], "object": "batch", "status": b["status"],
                   "input_file_id": b["input_file_id"], "request_counts": b["counts"],
                   "created_at": int(time.time())}
            if b["status"] == "completed":
                out["output_file_id"] = b["output_file_id"]
            return self._json(200, out)
        if "/files/" in p and p.endswith("/content"):
            fid = p.split("/files/")[1].rsplit("/content", 1)[0]
            path = FILES.get(fid)
            if not path or not os.path.exists(path):
                return self._json(404, {"error": {"message": "no file"}})
            data = open(path, "rb").read()
            self.send_response(200)
            self.send_header("content-type", "application/jsonl")
            self.send_header("content-length", str(len(data)))
            self.end_headers()
            return self.wfile.write(data)
        self._json(404, {"error": {"message": "no route " + self.path}})

    def log_message(self, *a):
        pass


if __name__ == "__main__":
    print("shim on 127.0.0.1:%d key=%s model=%s" % (PORT, "set" if KEY else "MISSING", DEFAULT_MODEL), flush=True)
    ThreadingHTTPServer(("127.0.0.1", PORT), H).serve_forever()
