# Go NLQ Service (OpenLLaMA → PostgreSQL)

Natural-language to **safe SQL** for your LAB database.
This service sends a prompt (your schema + user question) to a local LLM server (llama.cpp), sanitizes/repairs the generated SQL, runs it **read-only** on Postgres, and returns JSON.

## ✨ Features

* Chat → Completion fallback with JSON-only prompting
* Auto-quoting for text/uuid/date/timestamp columns (fixes `lab_id = 6666` → `lab_id = '6666'`)
* Partial-JSON finisher (handles truncated LLM output)
* Read-only transaction + `EXPLAIN` preflight for safety
* Schema filtering & limits (smaller prompt = faster, more reliable)
* Logs **every** raw LLM response to `logs/llm_raw.log`

---

## 1) Prereqs

* **Go** 1.20+
* A running **llama.cpp** HTTP server (e.g. `llama-server` on `http://127.0.0.1:8080`) serving an instruction-tuned or OpenLLaMA model
* Postgres reachable from this host

> Tip: you already have the `run-openllama.sh` helper; if not, run your llama server manually.

---

## 2) Configure environment

Create `.env` in this folder:

```env
# REQUIRED
POSTGRES_URL=postgres://postgres:password@0.0.0.0:5439/lab?sslmode=disable
LLM_HOST=http://127.0.0.1:8080

# OPTIONAL TUNING
LLM_TIMEOUT_SECONDS=240
LLM_NPREDICT=256
SCHEMA_LIMIT=120
SCHEMA_HINTS=patient,test,lab,department
```

**Notes**

* Use a **read-only** Postgres user in production.
* If your password has special characters, URL-encode them.

---

## 3) Build & run

```bash
# load env
set -a; source .env; set +a

# build
go build -o nlq-server .

# run (foreground)
./nlq-server

# or run in background with logs
mkdir -p logs
nohup ./nlq-server > logs/nlq_server.log 2>&1 &
echo $! > logs/nlq_server.pid
```

The server listens on **`:8090`**.

---

## 4) Test the API

**Endpoint**: `POST /nlq`
**Body**:

```json
{
  "question": "give me all data of patient whose lab_id is 6666",
  "limit": 100
}
```

**Curl**

```bash
curl -s http://127.0.0.1:8090/nlq \
  -H 'content-type: application/json' \
  -d '{"question":"give me all data of patient whose lab_id is 6666","limit":100}' | jq
```

**Response (example)**

```json
{
  "sql": "SELECT * FROM public.patient_tests WHERE lab_id = '6666' LIMIT 100",
  "columns": ["id","hospital_no","lab_id", "..."],
  "rows": [
    ["4171048","H-123","6666", "..."]
  ]
}
```

If something goes wrong you’ll get:

```json
{ "error": "parse error: invalid JSON from LLM" }
```

or

```json
{ "error": "query error: ERROR: operator does not exist: character varying = integer ..." }
```

---

## 5) What the service does under the hood

* Introspects your schema from `information_schema.columns` (limited by `SCHEMA_LIMIT` and filtered by `SCHEMA_HINTS`)
* Builds a compact system/user prompt that **forces JSON** (`{"sql":"..."}`) and prefers `SELECT *`
* Calls the LLM:

  1. `/v1/chat/completions` with `response_format: {"type":"json_object"}`
  2. falls back to `/completion` if needed
* Repairs partial JSON if truncated
* **Type-aware quoting**: turns `col = 123` → `col = '123'` when `col` is text/uuid/date/timestamp (also for `IN (..)` lists)
* Opens a **read-only** transaction, sets `statement_timeout = '5s'`, does an `EXPLAIN` preflight, then executes the query
* Returns `{"sql","columns","rows"}`

All prompts & raw LLM responses are appended to:

```
logs/llm_raw.log
```

---

## 6) Troubleshooting

* **`llm error: context deadline exceeded`**
  Increase `LLM_TIMEOUT_SECONDS` and/or reduce `SCHEMA_LIMIT`. Ensure your llama server is responsive:

  ```bash
  curl -s http://127.0.0.1:8080/health
  ```

* **`parse error: invalid JSON from LLM`**
  The base model isn’t following JSON. Try:

  * Set `SCHEMA_LIMIT=100` and keep `SCHEMA_HINTS` tight (patient,test,lab)
  * Use an **instruction-tuned** model in llama.cpp for more reliable JSON

* **`operator does not exist: character varying = integer`**
  Our auto-quoter should fix this. If it still appears, check `logs/llm_raw.log` to see the original SQL and confirm column types.

* **No output / not listening on 8090**
  Check logs:

  ```bash
  tail -n 200 logs/nlq_server.log
  ```

* **Change port**
  Edit the `Addr: ":8090"` in `main.go`, rebuild, restart.

---

## 7) Security & safety

* The service **refuses non-SELECT** queries and rejects any query with `;` or write keywords.
* Use a **read-only** DB role with limited schema permissions.
* Log files may include schema snippets and model outputs—store them securely.

---

## 8) Handy commands

```bash
# stop background server
kill $(cat logs/nlq_server.pid)

# see what's listening
ss -ltnp | grep -E ':8090|:8080' || lsof -i :8090

# tail both logs
tail -n +1 -v logs/nlq_server.log logs/llm_raw.log
```

---

## 9) Request patterns that work best

* Be explicit with filters:
  “all patient tests **where lab\_id is 6666** within last 7 days”
* Mention expected table names/fields if you know them
* Include the desired limit if you want more/less than the default

---

## 10) Roadmap (optional next steps)

* Swap in a stronger **instruct** model for higher JSON reliability
* Add **parameterization** to transform generated SQL into prepared statements
* Persist query history and add audit metadata
* Add `/healthz` endpoint and Prometheus metrics

---

**Author:** Bidhan Baniya ([https://github.com/bidhan948](https://github.com/bidhan948))
**Service:** Go NLQ (OpenLLaMA + Postgres)

---
