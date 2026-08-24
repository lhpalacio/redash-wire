package redash

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// DefaultMaxResultBytes bounds how many bytes of a query-result response the proxy
// will read into memory, so one enormous SELECT cannot OOM the process (and every
// other session with it).
const DefaultMaxResultBytes int64 = 256 << 20

var errResultTooLarge = errors.New("query result exceeds the maximum allowed size")

// maxBytesReader returns an error once more than limit bytes have been read,
// instead of silently truncating (which would surface as a confusing JSON error).
type maxBytesReader struct {
	r         io.Reader
	remaining int64
}

func newMaxBytesReader(r io.Reader, limit int64) *maxBytesReader {
	return &maxBytesReader{r: r, remaining: limit + 1}
}

func (m *maxBytesReader) Read(p []byte) (int, error) {
	if m.remaining <= 0 {
		return 0, errResultTooLarge
	}
	if int64(len(p)) > m.remaining {
		p = p[:m.remaining]
	}
	n, err := m.r.Read(p)
	m.remaining -= int64(n)
	return n, err
}

// httpError carries the HTTP status of a failed Redash API call so the poll loop
// can distinguish fatal responses (401/403/404) from transient ones (5xx/429).
type httpError struct {
	statusCode int
	message    string
}

func (e *httpError) Error() string { return e.message }

func isFatalStatus(code int) bool {
	return code == http.StatusUnauthorized || code == http.StatusForbidden || code == http.StatusNotFound
}

// HTTPStatus reports the status of a failed API call, and false when the error
// came from the transport instead of a response.
func HTTPStatus(err error) (int, bool) {
	var he *httpError
	if errors.As(err, &he) {
		return he.statusCode, true
	}
	return 0, false
}

// QueryError represents a failure of the SQL query itself (as reported by the
// data source via Redash), as opposed to an infrastructure failure talking to
// Redash. Its message is safe to surface to the SQL client; infrastructure errors
// are not, since they can leak internal hostnames/credentials.
type QueryError struct {
	Message string
}

func (e *QueryError) Error() string { return e.Message }

type Column struct {
	Name         string `json:"name"`
	FriendlyName string `json:"friendly_name"`
	Type         string `json:"type"`
}

type QueryResult struct {
	Columns []Column
	Rows    []map[string]any
}

type DataSource struct {
	ID   int    `json:"id"`
	Name string `json:"name"`
	Type string `json:"type"`
}

type SchemaTable struct {
	Name    string   `json:"name"`
	Columns []string `json:"-"`
}

type schemaTableRaw struct {
	Name    string            `json:"name"`
	Columns []json.RawMessage `json:"columns"`
}

func (r *schemaTableRaw) toSchemaTable() SchemaTable {
	t := SchemaTable{Name: r.Name}
	for _, raw := range r.Columns {
		var s string
		if json.Unmarshal(raw, &s) == nil {
			t.Columns = append(t.Columns, s)
			continue
		}
		var obj struct {
			Name string `json:"name"`
		}
		if json.Unmarshal(raw, &obj) == nil {
			t.Columns = append(t.Columns, obj.Name)
		}
	}
	return t
}

type Client struct {
	httpClient     *http.Client
	baseURL        string
	apiKey         string
	pollInterval   time.Duration
	pollTimeout    time.Duration
	maxResultBytes int64
}

type ClientOption func(*Client)

func WithMaxResultBytes(n int64) ClientOption {
	return func(c *Client) { c.maxResultBytes = n }
}

func WithPollInterval(d time.Duration) ClientOption {
	return func(c *Client) { c.pollInterval = d }
}

func WithPollTimeout(d time.Duration) ClientOption {
	return func(c *Client) { c.pollTimeout = d }
}

func WithHTTPClient(hc *http.Client) ClientOption {
	return func(c *Client) { c.httpClient = hc }
}

func NewClient(baseURL, apiKey string, opts ...ClientOption) *Client {
	c := &Client{
		httpClient:     &http.Client{Timeout: 30 * time.Second},
		baseURL:        baseURL,
		apiKey:         apiKey,
		pollInterval:   500 * time.Millisecond,
		pollTimeout:    120 * time.Second,
		maxResultBytes: DefaultMaxResultBytes,
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

type SessionInfo struct {
	User struct {
		Name  string `json:"name"`
		Email string `json:"email"`
	} `json:"user"`
	ClientConfig struct {
		Version string `json:"version"`
	} `json:"client_config"`
}

func (c *Client) GetSession(ctx context.Context) (*SessionInfo, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/api/session", nil)
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}
	req.Header.Set("Authorization", "Key "+c.apiKey)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetching session: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, &httpError{statusCode: resp.StatusCode, message: fmt.Sprintf("session request failed (status %d)", resp.StatusCode)}
	}

	var info SessionInfo
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
		return nil, fmt.Errorf("decoding session: %w", err)
	}

	return &info, nil
}

func (c *Client) ListDataSources(ctx context.Context) ([]DataSource, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/api/data_sources", nil)
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}
	req.Header.Set("Authorization", "Key "+c.apiKey)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetching data sources: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, &httpError{statusCode: resp.StatusCode, message: fmt.Sprintf("data sources request failed (status %d)", resp.StatusCode)}
	}

	var sources []DataSource
	if err := json.NewDecoder(resp.Body).Decode(&sources); err != nil {
		return nil, fmt.Errorf("decoding data sources: %w", err)
	}

	return sources, nil
}

func (c *Client) GetSchema(ctx context.Context, dataSourceID int) ([]SchemaTable, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		fmt.Sprintf("%s/api/data_sources/%d/schema", c.baseURL, dataSourceID), nil)
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}
	req.Header.Set("Authorization", "Key "+c.apiKey)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetching schema: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("schema request failed (status %d)", resp.StatusCode)
	}

	var envelope struct {
		Schema []schemaTableRaw `json:"schema"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&envelope); err != nil {
		return nil, fmt.Errorf("decoding schema: %w", err)
	}

	tables := make([]SchemaTable, len(envelope.Schema))
	for i, raw := range envelope.Schema {
		tables[i] = raw.toSchemaTable()
	}
	return tables, nil
}

type queryRequest struct {
	Query        string `json:"query"`
	DataSourceID int    `json:"data_source_id"`
	MaxAge       int    `json:"max_age"`
}

type queryResultEnvelope struct {
	QueryResult *struct {
		Data struct {
			Columns []Column          `json:"columns"`
			Rows    []json.RawMessage `json:"rows"`
		} `json:"data"`
	} `json:"query_result"`
	Job *jobInfo `json:"job"`
}

type jobInfo struct {
	ID            string `json:"id"`
	Status        int    `json:"status"`
	Error         string `json:"error"`
	QueryResultID *int   `json:"query_result_id"`
}

type jobEnvelope struct {
	Job jobInfo `json:"job"`
}

const (
	jobStatusQueued   = 1
	jobStatusStarted  = 2
	jobStatusFinished = 3
	jobStatusFailed   = 4
)

func (c *Client) ExecuteQuery(ctx context.Context, sql string, dataSourceID int) (*QueryResult, error) {
	body, err := json.Marshal(queryRequest{
		Query:        sql,
		DataSourceID: dataSourceID,
		MaxAge:       0,
	})
	if err != nil {
		return nil, fmt.Errorf("marshaling request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/api/query_results", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Key "+c.apiKey)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("executing request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("redash API error (status %d): %s", resp.StatusCode, redashErrorMessage(resp.Body))
	}

	var envelope queryResultEnvelope
	dec := json.NewDecoder(newMaxBytesReader(resp.Body, c.maxResultBytes))
	dec.UseNumber()
	if err := dec.Decode(&envelope); err != nil {
		return nil, fmt.Errorf("decoding response: %w", err)
	}

	if envelope.QueryResult != nil {
		return c.parseQueryResult(envelope.QueryResult.Data.Columns, envelope.QueryResult.Data.Rows)
	}

	if envelope.Job != nil {
		return c.pollJob(ctx, envelope.Job.ID)
	}

	return nil, fmt.Errorf("unexpected response: no query_result or job")
}

// maxConsecutivePollErrors bounds how many transient job-status failures in a row
// are tolerated before giving up, so a brief Redash/network blip mid-poll does not
// discard a query that is otherwise progressing.
const maxConsecutivePollErrors = 5

func (c *Client) pollJob(ctx context.Context, jobID string) (*QueryResult, error) {
	deadline := time.After(c.pollTimeout)
	ticker := time.NewTicker(c.pollInterval)
	defer ticker.Stop()

	consecutiveErrors := 0

	for {
		select {
		case <-ctx.Done():
			// The client went away (disconnect or shutdown). Tell Redash to stop the
			// job so the underlying warehouse query is not left running.
			c.cancelJob(jobID)
			return nil, ctx.Err()
		case <-deadline:
			c.cancelJob(jobID)
			return nil, fmt.Errorf("query timed out after %s", c.pollTimeout)
		case <-ticker.C:
			job, err := c.getJob(ctx, jobID)
			if err != nil {
				// Fatal responses (auth/not-found) abort immediately; everything else
				// (5xx, 429, transport blips) is transient: the job keeps running on
				// Redash, so retry on the next tick up to a bounded count.
				var he *httpError
				if errors.As(err, &he) && isFatalStatus(he.statusCode) {
					return nil, err
				}
				if ctx.Err() != nil {
					c.cancelJob(jobID)
					return nil, ctx.Err()
				}
				consecutiveErrors++
				if consecutiveErrors >= maxConsecutivePollErrors {
					return nil, fmt.Errorf("job polling failed %d times in a row: %w", consecutiveErrors, err)
				}
				continue
			}
			consecutiveErrors = 0

			switch job.Status {
			case jobStatusQueued, jobStatusStarted:
				continue
			case jobStatusFinished:
				if job.QueryResultID == nil {
					return &QueryResult{}, nil
				}
				return c.getQueryResult(ctx, *job.QueryResultID)
			case jobStatusFailed:
				if isNoDataError(job.Error) {
					return &QueryResult{}, nil
				}
				// A failed job is a SQL/query error from the data source, safe to
				// return to the client.
				return nil, &QueryError{Message: job.Error}
			default:
				return nil, fmt.Errorf("unknown job status: %d", job.Status)
			}
		}
	}
}

// cancelJob makes a best-effort request to Redash to cancel an in-flight job. It
// uses a fresh short-lived context because the caller's context is typically
// already cancelled or timed out when this runs.
func (c *Client) cancelJob(jobID string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, fmt.Sprintf("%s/api/jobs/%s", c.baseURL, jobID), nil)
	if err != nil {
		return
	}
	req.Header.Set("Authorization", "Key "+c.apiKey)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return
	}
	_ = resp.Body.Close()
}

func (c *Client) getJob(ctx context.Context, jobID string) (*jobInfo, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("%s/api/jobs/%s", c.baseURL, jobID), nil)
	if err != nil {
		return nil, fmt.Errorf("creating job request: %w", err)
	}
	req.Header.Set("Authorization", "Key "+c.apiKey)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("getting job status: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, &httpError{statusCode: resp.StatusCode, message: fmt.Sprintf("job status request failed (status %d)", resp.StatusCode)}
	}

	var envelope jobEnvelope
	if err := json.NewDecoder(resp.Body).Decode(&envelope); err != nil {
		return nil, fmt.Errorf("decoding job response: %w", err)
	}

	return &envelope.Job, nil
}

func (c *Client) getQueryResult(ctx context.Context, resultID int) (*QueryResult, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("%s/api/query_results/%d", c.baseURL, resultID), nil)
	if err != nil {
		return nil, fmt.Errorf("creating result request: %w", err)
	}
	req.Header.Set("Authorization", "Key "+c.apiKey)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("getting query result: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("query result request failed (status %d)", resp.StatusCode)
	}

	var envelope queryResultEnvelope
	dec := json.NewDecoder(newMaxBytesReader(resp.Body, c.maxResultBytes))
	dec.UseNumber()
	if err := dec.Decode(&envelope); err != nil {
		return nil, fmt.Errorf("decoding result response: %w", err)
	}

	if envelope.QueryResult == nil {
		return nil, fmt.Errorf("no query_result in response")
	}

	return c.parseQueryResult(envelope.QueryResult.Data.Columns, envelope.QueryResult.Data.Rows)
}

func isNoDataError(msg string) bool {
	return strings.Contains(strings.ToLower(msg), "query completed but it returned no data")
}

// redashErrorMessage extracts a concise message from a Redash JSON error body for
// server-side logging, rather than dumping the entire decoded payload.
func redashErrorMessage(body io.Reader) string {
	var e struct {
		Message string `json:"message"`
		Error   string `json:"error"`
	}
	if json.NewDecoder(newMaxBytesReader(body, 1<<20)).Decode(&e) == nil {
		switch {
		case e.Message != "":
			return e.Message
		case e.Error != "":
			return e.Error
		}
	}
	return "no message body"
}

func (c *Client) parseQueryResult(columns []Column, rawRows []json.RawMessage) (*QueryResult, error) {
	rows := make([]map[string]any, 0, len(rawRows))
	for _, raw := range rawRows {
		row := make(map[string]any)
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.UseNumber()
		if err := dec.Decode(&row); err != nil {
			return nil, fmt.Errorf("decoding row: %w", err)
		}
		rows = append(rows, row)
	}

	return &QueryResult{
		Columns: columns,
		Rows:    rows,
	}, nil
}
