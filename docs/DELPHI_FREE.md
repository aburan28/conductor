# Free endpoints for Delphi

How to run Delphi so that nothing costs money: models you host yourself on a free notebook
GPU, models on your own machine, and third-party free tiers. Free mode itself
(`DELPHI_FREE`, substitutes, the engine's refusal of paid requests) is described in
`docs/DELPHI.md`, "Free mode"; this page is the practical setup.

| Route | Cost | Privacy | Use it for |
|---|---|---|---|
| Your machine (vLLM, LM Studio, Ollama) | your electricity | private: prompts stay with you | anything |
| A notebook GPU you run (Colab, Kaggle) behind a tunnel | free tier of the notebook | private to you and the notebook platform | anything you would put in a notebook |
| Third-party free tiers (OpenRouter `:free` models) | free, rate-limited | **the provider may log or train on prompts** | public or synthetic content only |

`DELPHI_FREE=local` allows the first two; `DELPHI_FREE=public` adds the third. For research
work (unpublished results, designs, attack ideas), stay on `local`.

## 1. A Colab GPU running vLLM

The notebook runs an OpenAI-compatible vLLM server, a tunnel gives it an https address, and
Delphi reaches it as a `self_hosted` endpoint behind an API key you generate.

**Before you start:** free notebook tiers limit session length, may disconnect idle sessions,
do not guarantee a GPU, and have terms of use that restrict some uses. Read the platform's
current terms and check that serving a model to your own client through a tunnel is allowed
for your account. Kaggle notebooks work the same way and have their own GPU quota and terms.

### In the notebook (Runtime → Change runtime type → a GPU)

```python
# Cell 1: what GPU did you get?
!nvidia-smi --query-gpu=name,memory.total --format=csv
```

Pick a model that fits that memory with room for the KV cache. On a 16 GB card that means
roughly a 7–8B model quantized to 4 bits (an AWQ or GPTQ checkpoint), or a 3–4B model in
16-bit. Older cards such as the T4 have no bfloat16 support, hence `--dtype half` below.

```python
# Cell 2: install vLLM (takes a few minutes) and make an API key
!pip install -q vllm
import secrets; KEY = secrets.token_hex(32); print(KEY)   # copy this; it guards your GPU
```

```python
# Cell 3: serve the model in the background
import subprocess
MODEL = "<a Hugging Face model id that fits the GPU>"
server = subprocess.Popen(
    ["vllm", "serve", MODEL, "--served-model-name", "nb-model", "--api-key", KEY,
     "--dtype", "half", "--max-model-len", "8192", "--gpu-memory-utilization", "0.90",
     "--port", "8000"],
    stdout=open("vllm.log", "w"), stderr=subprocess.STDOUT)
```

```python
# Cell 4: wait until it answers locally
!sleep 60; curl -s -H "Authorization: Bearer {KEY}" http://localhost:8000/v1/models; tail -5 vllm.log
```

```python
# Cell 5: an https tunnel (a Cloudflare quick tunnel: no account, a new URL each time,
# meant for testing, with no uptime guarantee)
!wget -q https://github.com/cloudflare/cloudflared/releases/latest/download/cloudflared-linux-amd64 -O cloudflared && chmod +x cloudflared
tunnel = subprocess.Popen(["./cloudflared", "tunnel", "--url", "http://localhost:8000"],
                          stdout=open("tunnel.log", "w"), stderr=subprocess.STDOUT)
!sleep 10; grep -o 'https://[a-z0-9-]*\.trycloudflare\.com' tunnel.log | head -1
```

The URL is public: the API key is the only thing keeping others off your GPU. Never print
the key in a notebook you share, and stop the tunnel when you are done.

### On your machine

In `~/.delphi/endpoints.json`:

```json
{"colab": {"base_url": "https://<name>.trycloudflare.com/v1", "key_env": "COLAB_VLLM_KEY",
           "self_hosted": true, "default_model": "nb-model", "concurrency": 2}}
```

`self_hosted: true` tells Delphi the endpoint is yours, so free mode counts it as free and
private even though it has a key. Then:

```sh
export COLAB_VLLM_KEY=<the key from cell 2>
delphi providers                                  # colab ... ready
DELPHI_FREE=local delphi run -provider colab -prompt "Reply with the word OK." -out /tmp/colab-check
```

A quick tunnel's URL changes every time it starts; update `base_url` when it does. If the
session drops mid-flight, Delphi's spool rule applies: a flight with no spool file is requeued
with `delphi reconcile -label L -requeue` once the endpoint is back.

## 2. Your own machine

Run vLLM, LM Studio or Ollama locally and name it in the endpoints file; local presets need
no key:

```json
{"vllm": {"base_url": "http://127.0.0.1:8000/v1", "default_model": "<served model name>"}}
```

## 3. OpenRouter free models (public content only)

Create an OpenRouter account and key, then pick a model whose id ends in `:free` from
OpenRouter's model list. Free models are rate-limited, can be withdrawn without notice, and
their providers may log or train on prompts; check each model's page for its data policy.

```sh
export OPENROUTER_API_KEY=...
DELPHI_FREE=public delphi run -provider openrouter -model <vendor/model:free> -prompt "..." -out run/
```

OpenCode's free models (`*-free`) are not an option here: OpenCode documents them as usable
only inside OpenCode, not from other clients.

## 4. Route everything free

Map each paid model your workflows ask for to a free one in `~/.delphi/free.json`:

```json
{"substitutes": {
  "anthropic:claude-sonnet-5-5": "colab:nb-model",
  "anthropic:claude-haiku-5-5": "vllm:<small local model>",
  "*": "colab:nb-model"
}}
```

With `DELPHI_FREE=local`, `task_card.py lane --via delphi`, the goal loop, `run` and
`calibrate` then answer from your own GPUs, and the engine refuses anything that would cost
money.

## 5. Check a free model before trusting it

A free substitute is a different, usually smaller, model. Measure it on the work it will do
before relying on it:

```sh
DELPHI_FREE=local delphi calibrate -suite evals/calibration/card-compile.json -provider colab
delphi registry table -family card-compile
```

(The director suites live in crypto-autoresearcher, `evals/calibration/`.) If it is
`worse` or `inconclusive` against the model it replaces, keep the paid model for that job.
