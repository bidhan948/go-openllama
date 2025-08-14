package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

/* ---------- Types ---------- */

type chatMsg struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}
type chatReq struct {
	Messages       []chatMsg   `json:"messages"`
	Temperature    float64     `json:"temperature,omitempty"`
	MaxTokens      int         `json:"max_tokens,omitempty"`
	ResponseFormat interface{} `json:"response_format,omitempty"`
}
type chatResp struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	} `json:"choices"`
}

type compReq struct {
	Prompt      string   `json:"prompt"`
	Stream      bool     `json:"stream"`
	NPredict    int      `json:"n_predict,omitempty"`
	Temperature float64  `json:"temperature,omitempty"`
	Stop        []string `json:"stop,omitempty"`
}
type compResp struct {
	Content string `json:"content"`
}

type nlqReq struct {
	Question string `json:"question"`
	Limit    int    `json:"limit,omitempty"`
}
type nlqResp struct {
	SQL     string     `json:"sql"`
	Columns []string   `json:"columns"`
	Rows    [][]string `json:"rows"`
}

/* ---------- Globals ---------- */

var (
	writeKw  = regexp.MustCompile(`(?i)\b(INSERT|UPDATE|DELETE|DROP|ALTER|TRUNCATE|GRANT|REVOKE|CREATE|VACUUM|ANALYZE|COPY|SET|SHOW)\b`)
	semiCol  = regexp.MustCompile(`;`)
	selectRe = regexp.MustCompile(`(?is)\bselect\b[\s\S]*$`)
	llmMu    sync.Mutex
)

/* ---------- Utils ---------- */

func envInt(key string, def int) int {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return def
}

func isSafe(q string) bool {
	s := strings.TrimSpace(q)
	if !strings.HasPrefix(strings.ToUpper(s), "SELECT") {
		return false
	}
	return !writeKw.MatchString(s) && !semiCol.MatchString(s)
}

func ensureLogsDir() { _ = os.MkdirAll("logs", 0o755) }

func logLLM(kind, prompt string, status int, raw []byte, reqErr error) {
	ensureLogsDir()
	llmMu.Lock()
	defer llmMu.Unlock()
	f, err := os.OpenFile("logs/llm_raw.log", os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	ts := time.Now().Format(time.RFC3339)
	if len(prompt) > 2000 {
		prompt = prompt[:2000] + "…(truncated)"
	}
	fmt.Fprintf(f, "==== %s %s ====\nSTATUS: %d\n", ts, strings.ToUpper(kind), status)
	if reqErr != nil {
		fmt.Fprintf(f, "ERROR: %v\n", reqErr)
	}
	fmt.Fprintf(f, "PROMPT:\n%s\n", prompt)
	fmt.Fprintln(f, "RAW:")
	f.Write(raw)
	fmt.Fprintln(f, "\n")
}

/* ---------- LLM calls ---------- */

func askChat(ctx context.Context, host, system, user string, maxTok int) (string, error) {
	body, _ := json.Marshal(chatReq{
		Messages: []chatMsg{
			{Role: "system", Content: system},
			{Role: "user", Content: user},
		},
		Temperature: 0,
		MaxTokens:   maxTok,
		ResponseFormat: map[string]any{
			"type": "json_object",
		},
	})
	req, _ := http.NewRequestWithContext(ctx, "POST", host+"/v1/chat/completions", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if resp != nil {
		defer resp.Body.Close()
	}
	var raw []byte
	status := 0
	if resp != nil {
		status = resp.StatusCode
		rb, _ := io.ReadAll(resp.Body)
		raw = rb
	}
	logLLM("chat", "SYSTEM:\n"+system+"\n\nUSER:\n"+user, status, raw, err)
	if err != nil {
		return "", err
	}
	var out chatResp
	if err := json.Unmarshal(raw, &out); err != nil || len(out.Choices) == 0 {
		return "", errors.New("bad chat response")
	}
	return out.Choices[0].Message.Content, nil
}

func askCompletion(ctx context.Context, host, prompt string, nPredict int) (string, error) {
	body, _ := json.Marshal(compReq{
		Prompt:      prompt,
		Stream:      false,
		NPredict:    nPredict,
		Temperature: 0,
	})
	req, _ := http.NewRequestWithContext(ctx, "POST", host+"/completion", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if resp != nil {
		defer resp.Body.Close()
	}
	var raw []byte
	status := 0
	if resp != nil {
		status = resp.StatusCode
		rb, _ := io.ReadAll(resp.Body)
		raw = rb
	}
	logLLM("completion", prompt, status, raw, err)
	if err != nil {
		return "", err
	}
	var out compResp
	if err := json.Unmarshal(raw, &out); err == nil && strings.TrimSpace(out.Content) != "" {
		return out.Content, nil
	}
	return string(raw), nil
}

/* ---------- Partial JSON finisher (no grammar) ---------- */

func finishJSONWithLLM(ctx context.Context, host, partial string) (string, error) {
	p := "Finish this JSON object. Return ONLY the completed JSON, no extra text:\n" + partial
	return askCompletion(ctx, host, p, 96)
}

func ensureClosedJSON(ctx context.Context, host, raw string) string {
	s := strings.TrimSpace(raw)
	if strings.Count(s, "{") > strings.Count(s, "}") {
		if done, err := finishJSONWithLLM(ctx, host, s); err == nil && strings.Count(done, "{") <= strings.Count(done, "}") {
			return done
		}
	}
	return raw
}

/* ---------- Schema helpers ---------- */

func buildSchemaQuery(limit int, hints []string) (string, []any) {
	base := `
SELECT table_schema, table_name, column_name, data_type
FROM information_schema.columns
WHERE table_schema NOT IN ('pg_catalog','information_schema')
`
	args := []any{}
	if len(hints) > 0 {
		parts := []string{}
		i := 1
		for _, h := range hints {
			h = strings.TrimSpace(h)
			if h == "" {
				continue
			}
			parts = append(parts, fmt.Sprintf("(table_name ILIKE $%d OR column_name ILIKE $%d)", i, i))
			args = append(args, "%"+h+"%")
			i++
		}
		if len(parts) > 0 {
			base += "AND (" + strings.Join(parts, " OR ") + ")\n"
		}
	}
	base += "ORDER BY table_schema, table_name, ordinal_position\n"
	base += fmt.Sprintf("LIMIT %d;", limit)
	return base, args
}

func getSchema(ctx context.Context, db *pgxpool.Pool, limit int, hintsCSV string) (string, error) {
	hints := []string{}
	for _, p := range strings.Split(hintsCSV, ",") {
		t := strings.TrimSpace(p)
		if t != "" {
			hints = append(hints, t)
		}
	}
	q, args := buildSchemaQuery(limit, hints)
	rows, err := db.Query(ctx, q, args...)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	var sb strings.Builder
	cur := ""
	for rows.Next() {
		var s, t, c, dt string
		if err := rows.Scan(&s, &t, &c, &dt); err != nil {
			return "", err
		}
		full := s + "." + t
		if full != cur {
			if cur != "" {
				sb.WriteString("]\n")
			}
			cur = full
			sb.WriteString(fmt.Sprintf("- %s [", full))
		} else {
			sb.WriteString(", ")
		}
		sb.WriteString(fmt.Sprintf("%s:%s", c, dt))
	}
	if cur != "" {
		sb.WriteString("]\n")
	}
	return sb.String(), rows.Err()
}

func getTypeMap(ctx context.Context, db *pgxpool.Pool) (map[string]string, error) {
	q := `
SELECT table_schema, table_name, column_name, data_type
FROM information_schema.columns
WHERE table_schema NOT IN ('pg_catalog','information_schema');
`
	rows, err := db.Query(ctx, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	m := make(map[string]string, 1024)
	for rows.Next() {
		var s, t, c, dt string
		if err := rows.Scan(&s, &t, &c, &dt); err != nil {
			return nil, err
		}
		m[c] = dt
		m[t+"."+c] = dt
		m[s+"."+t+"."+c] = dt
	}
	return m, rows.Err()
}

func isTextualType(t string) bool {
	t = strings.ToLower(t)
	switch t {
	case "character varying", "varchar", "text", "uuid",
		"date", "time without time zone", "time with time zone",
		"timestamp without time zone", "timestamp with time zone":
		return true
	default:
		return false
	}
}

func fixSQLTypes(sql string, types map[string]string) string {
	type colpat struct {
		col  string
		reEq *regexp.Regexp
		reIn *regexp.Regexp
	}
	var pats []colpat
	seen := map[string]bool{}
	for k, dt := range types {
		if !isTextualType(dt) {
			continue
		}
		parts := strings.Split(k, ".")
		col := parts[len(parts)-1]
		if seen[col] {
			continue
		}
		seen[col] = true
		reEq := regexp.MustCompile(`(?i)(\b(?:[a-z_][a-z0-9_]*\.)*` + regexp.QuoteMeta(col) + `\b)\s*=\s*([0-9]+)\b`)
		reIn := regexp.MustCompile(`(?i)(\b(?:[a-z_][a-z0-9_]*\.)*` + regexp.QuoteMeta(col) + `\b)\s+IN\s*\(([^)]*)\)`)
		pats = append(pats, colpat{col: col, reEq: reEq, reIn: reIn})
	}
	out := sql
	for _, p := range pats {
		out = p.reEq.ReplaceAllString(out, `$1 = '$2'`)
		out = p.reIn.ReplaceAllStringFunc(out, func(m string) string {
			sub := p.reIn.FindStringSubmatch(m)
			if len(sub) != 3 {
				return m
			}
			colExpr, list := sub[1], sub[2]
			parts := strings.Split(list, ",")
			for i := range parts {
				v := strings.TrimSpace(parts[i])
				if v == "" {
					continue
				}
				if (v[0] == '\'' && v[len(v)-1] == '\'') || (v[0] == '"' && v[len(v)-1] == '"') {
					continue
				}
				allDigits := true
				for _, r := range v {
					if !unicode.IsDigit(r) {
						allDigits = false
						break
					}
				}
				if allDigits {
					parts[i] = "'" + v + "'"
				}
			}
			return fmt.Sprintf("%s IN (%s)", colExpr, strings.Join(parts, ", "))
		})
	}
	return out
}

/* ---------- Prompting ---------- */

func buildSystem(limit int) string {
	if limit <= 0 {
		limit = 100
	}
	return fmt.Sprintf(`You output only JSON like {"sql":"SELECT ..."}.
Rules:
- Single SELECT, no semicolons.
- Use only provided schema (table.column:type).
- Prefer SELECT * unless the user asked for specific columns.
- If a column type is character varying/text/uuid/date/time/timestamp, quote the literal: e.g. WHERE lab_id = '6666'.
- Include LIMIT %d unless user asked otherwise.`, limit)
}

func buildUser(schema, question string) string {
	fewshot := `Example:
Schema:
- public.patient_tests [lab_id:character varying, patient_id:bigint]
User question: give me all data of patient whose lab_id is 6666
JSON:
{"sql":"SELECT * FROM public.patient_tests WHERE lab_id = '6666' LIMIT 100"}`
	return fmt.Sprintf("%s\n\nSchema:\n%s\n\nUser question: %s\nJSON:", fewshot, schema, question)
}

func buildPrompt(schema, question string, limit int) string {
	if limit <= 0 {
		limit = 100
	}
	return fmt.Sprintf(`Return only: {"sql":"SELECT ..."} with a single SELECT and no semicolons. Use given schema. Include LIMIT %d unless user asked otherwise.

Schema:
%s

User question: %q
JSON:`, limit, schema, question)
}

/* ---------- Extraction & fallbacks ---------- */

func extractSQL(raw string) (string, error) {
	s := strings.TrimSpace(raw)
	s = strings.Trim(s, "`")
	start := strings.Index(s, "{")
	end := strings.LastIndex(s, "}")
	if start >= 0 && end > start {
		var obj struct {
			SQL string `json:"sql"`
		}
		if err := json.Unmarshal([]byte(s[start:end+1]), &obj); err == nil && strings.TrimSpace(obj.SQL) != "" {
			return obj.SQL, nil
		}
	}
	if m := selectRe.FindString(s); strings.TrimSpace(m) != "" {
		m = strings.TrimSpace(m)
		m = strings.TrimSuffix(m, "```")
		return m, nil
	}
	return "", errors.New("invalid JSON from LLM")
}

func heuristicSQL(question string, defLimit int) (string, bool) {
	if defLimit <= 0 {
		defLimit = 100
	}
	reLab := regexp.MustCompile(`(?i)\blab[_\s-]*id\b[^0-9]*([0-9]+)`)
	if m := reLab.FindStringSubmatch(question); len(m) == 2 {
		return fmt.Sprintf("SELECT * FROM public.patient_tests WHERE lab_id = '%s' LIMIT %d", m[1], defLimit), true
	}
	return "", false
}

/* ---------- PG exec ---------- */

func runReadOnly(ctx context.Context, db *pgxpool.Pool, q string) ([]string, [][]string, error) {
	if !isSafe(q) {
		return nil, nil, errors.New("unsafe SQL")
	}
	tx, err := db.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, nil, err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, "SET LOCAL statement_timeout = '5s'"); err != nil {
		return nil, nil, err
	}
	if _, err := tx.Exec(ctx, "EXPLAIN "+q); err != nil {
		return nil, nil, err
	}
	rows, err := tx.Query(ctx, q)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	fds := rows.FieldDescriptions()
	cols := make([]string, len(fds))
	for i, f := range fds {
		cols[i] = string(f.Name)
	}
	var out [][]string
	for rows.Next() {
		vals, err := rows.Values()
		if err != nil {
			return nil, nil, err
		}
		rec := make([]string, len(vals))
		for i, v := range vals {
			rec[i] = fmt.Sprint(v)
		}
		out = append(out, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, nil, err
	}
	return cols, out, nil
}

/* ---------- HTTP helpers ---------- */

func writeErr(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

/* ---------- main ---------- */

func main() {
	pgURL := os.Getenv("POSTGRES_URL")
	llmHost := os.Getenv("LLM_HOST")
	if pgURL == "" || llmHost == "" {
		fmt.Println("set POSTGRES_URL and LLM_HOST")
		os.Exit(1)
	}
	timeoutSec := envInt("LLM_TIMEOUT_SECONDS", 240)
	nPredict := envInt("LLM_NPREDICT", 256)
	schemaLimit := envInt("SCHEMA_LIMIT", 120)
	schemaHints := os.Getenv("SCHEMA_HINTS")
	if schemaHints == "" {
		schemaHints = "patient,test,lab,department"
	}

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, pgURL)
	if err != nil {
		panic(err)
	}
	defer pool.Close()

	mux := http.NewServeMux()
	mux.HandleFunc("/nlq", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			writeErr(w, 405, "method not allowed")
			return
		}
		var in nlqReq
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil || strings.TrimSpace(in.Question) == "" {
			writeErr(w, 400, "invalid request")
			return
		}
		qctx, cancel := context.WithTimeout(r.Context(), time.Duration(timeoutSec)*time.Second)
		defer cancel()

		schema, err := getSchema(qctx, pool, schemaLimit, schemaHints)
		if err != nil {
			writeErr(w, 500, "schema introspection failed: "+err.Error())
			return
		}

		sys := buildSystem(in.Limit)
		usr := buildUser(schema, in.Question)
		raw, err := askChat(qctx, llmHost, sys, usr, nPredict)
		if err != nil {
			prompt := buildPrompt(schema, in.Question, in.Limit)
			raw, err = askCompletion(qctx, llmHost, prompt, nPredict)
			if err != nil {
				// last-resort heuristic before failing
				if sq, ok := heuristicSQL(in.Question, in.Limit); ok {
					cols, rows, err2 := runReadOnly(qctx, pool, sq)
					if err2 == nil {
						w.Header().Set("Content-Type", "application/json")
						_ = json.NewEncoder(w).Encode(nlqResp{SQL: sq, Columns: cols, Rows: rows})
						return
					}
				}
				writeErr(w, 502, "llm error: "+err.Error())
				return
			}
		}

		raw = ensureClosedJSON(qctx, llmHost, raw)
		sql, err := extractSQL(raw)
		if err != nil {
			// try heuristic before failing
			if sq, ok := heuristicSQL(in.Question, in.Limit); ok {
				cols, rows, err2 := runReadOnly(qctx, pool, sq)
				if err2 == nil {
					w.Header().Set("Content-Type", "application/json")
					_ = json.NewEncoder(w).Encode(nlqResp{SQL: sq, Columns: cols, Rows: rows})
					return
				}
			}
			writeErr(w, 500, "parse error: invalid JSON from LLM")
			return
		}

		if typeMap, err := getTypeMap(qctx, pool); err == nil {
			sql = fixSQLTypes(sql, typeMap)
		}

		cols, rows, err := runReadOnly(qctx, pool, sql)
		if err != nil {
			writeErr(w, 400, "query error: "+err.Error())
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(nlqResp{SQL: sql, Columns: cols, Rows: rows})
	})

	srv := &http.Server{
		Addr:              ":8090",
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	fmt.Println("listening on :8090 POST /nlq")
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		panic(err)
	}
}
