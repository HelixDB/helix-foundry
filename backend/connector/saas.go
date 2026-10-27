package connector

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// get sends one authenticated GET and returns the status and up to 1 MiB of body.
func get(ctx context.Context, client *http.Client, url string, header http.Header) (int, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, nil, err
	}
	req.Header = header
	res, err := client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer res.Body.Close()
	body, err := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	return res.StatusCode, body, err
}

// postJSON sends one authenticated JSON POST and returns the status and up to 1 MiB of body.
func postJSON(ctx context.Context, client *http.Client, url string, header http.Header, body any) (int, []byte, error) {
	buf, err := json.Marshal(body)
	if err != nil {
		return 0, nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(buf))
	if err != nil {
		return 0, nil, err
	}
	req.Header = header.Clone()
	req.Header.Set("Content-Type", "application/json")
	res, err := client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer res.Body.Close()
	out, err := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	return res.StatusCode, out, err
}

// maxPageBytes bounds one list page; a PostHog page of 10,000 events is the largest.
const maxPageBytes = 64 << 20

// maxRetries is how many times fetch retries a rate limit or server error.
const maxRetries = 6

// retryBackoff waits Retry-After when the API gives it (capped at 30s), else
// doubles from 250ms. Tests replace it.
var retryBackoff = func(attempt int, retryAfter string) time.Duration {
	if seconds, err := strconv.Atoi(retryAfter); err == nil && seconds > 0 {
		return min(time.Duration(seconds)*time.Second, 30*time.Second)
	}
	return 250 * time.Millisecond << attempt
}

// fetch sends the request build makes, retrying 429 and 5xx answers. It
// returns the final status and body.
func fetch(ctx context.Context, client *http.Client, build func() (*http.Request, error)) (int, []byte, error) {
	status, _, body, err := fetchWithHeader(ctx, client, build)
	return status, body, err
}

// fetchWithHeader is fetch that also returns the final response's header. A
// 403 carrying Retry-After is a rate limit too: GitHub's secondary limit.
func fetchWithHeader(ctx context.Context, client *http.Client, build func() (*http.Request, error)) (int, http.Header, []byte, error) {
	for attempt := 0; ; attempt++ {
		req, err := build()
		if err != nil {
			return 0, nil, nil, err
		}
		res, err := client.Do(req.WithContext(ctx))
		if err != nil {
			return 0, nil, nil, err
		}
		body, err := io.ReadAll(io.LimitReader(res.Body, maxPageBytes))
		res.Body.Close()
		if err != nil {
			return 0, nil, nil, err
		}
		retryAfter := res.Header.Get("Retry-After")
		limited := res.StatusCode == http.StatusTooManyRequests || res.StatusCode >= 500 ||
			res.StatusCode == http.StatusForbidden && retryAfter != ""
		if limited && attempt < maxRetries {
			select {
			case <-ctx.Done():
				return 0, nil, nil, ctx.Err()
			case <-time.After(retryBackoff(attempt, retryAfter)):
			}
			continue
		}
		return res.StatusCode, res.Header, body, nil
	}
}

// probeAll calls probe for every path concurrently and returns the paths that
// were readable. The first refusal (a rejected key, an outage) wins.
func probeAll(ctx context.Context, paths []string, probe func(context.Context, string) (bool, error)) (map[string]bool, error) {
	var (
		mu       sync.Mutex
		wg       sync.WaitGroup
		readable = map[string]bool{}
		firstErr error
	)
	for _, path := range paths {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ok, err := probe(ctx, path)
			mu.Lock()
			defer mu.Unlock()
			if err != nil && firstErr == nil {
				firstErr = err
			}
			readable[path] = ok
		}()
	}
	wg.Wait()
	return readable, firstErr
}

// referencedBy builds foreign keys from each table's reference columns. A
// column names the table whose key it is; the first table with that key wins.
func referencedBy(objects []saasTable, columns map[string][]string) map[string][]ForeignKey {
	owner := map[string]string{}
	for _, o := range objects {
		if _, taken := owner[o.key]; !taken {
			owner[o.key] = o.name
		}
	}
	out := map[string][]ForeignKey{}
	for _, o := range objects {
		for _, column := range columns[o.name] {
			table, ok := owner[column]
			if !ok {
				panic(fmt.Sprintf("connector: %s.%s references no table", o.name, column))
			}
			out[o.name] = append(out[o.name], ForeignKey{Column: column, ReferencedTable: table, ReferencedColumn: column})
		}
	}
	return out
}

// emptySettings accepts a kind with no settings: absent, null or an empty object.
func emptySettings(raw json.RawMessage) error {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil || len(fields) != 0 {
		return invalid("this connector takes no settings besides its key")
	}
	return nil
}
