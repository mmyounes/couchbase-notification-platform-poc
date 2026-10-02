package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// FTS search over the notifications collection.
//
// Uses the FTS REST API rather than the SDK helper: the pagination design
// depends on `search_after`, whose behaviour was verified directly against
// Server 8.0.2 through this exact contract. The index stores no field bodies,
// so a query returns document ids which the caller hydrates via a bulk KV get.

type SearchFilters struct {
	Text, UserID, AppName, Channel, Status, Type string
	Seen                                         *bool
	FromISO, ToISO                               string
}

type SearchPage struct {
	IDs                  []string
	TotalHits            int64
	LastSort             string
	FailedPartitions     int
	SuccessfulPartitions int
}

type Search struct {
	baseURL, index, auth, tenant string
	client                       *http.Client
}

func NewSearch(baseURL, index, user, pass, tenant string) *Search {
	return &Search{
		baseURL: baseURL, index: index, tenant: tenant,
		auth:   "Basic " + base64.StdEncoding.EncodeToString([]byte(user+":"+pass)),
		client: &http.Client{Timeout: 90 * time.Second},
	}
}

func (s *Search) buildQuery(f SearchFilters) map[string]any {
	conj := []map[string]any{{"field": "tenant_id", "term": s.tenant}}

	if f.Text != "" {
		// `operator: and` is essential. FTS defaults a multi-term match to OR,
		// so "temporary password" would also return documents containing only
		// "password" - wrong for a filter box a user reads as a conjunction.
		conj = append(conj, map[string]any{
			"disjuncts": []map[string]any{
				{"field": "message", "match": f.Text, "operator": "and"},
				{"field": "subject", "match": f.Text, "operator": "and"},
			},
			"min": 1,
		})
	}
	add := func(field, val string) {
		if val != "" {
			conj = append(conj, map[string]any{"field": field, "term": val})
		}
	}
	add("user_id", f.UserID)
	add("app_name", f.AppName)
	add("channel", f.Channel)
	add("status", f.Status)
	add("type", f.Type)
	if f.Seen != nil {
		conj = append(conj, map[string]any{"field": "seen", "bool": *f.Seen})
	}
	if f.FromISO != "" || f.ToISO != "" {
		// Only send bounds actually supplied. FTS rejects out-of-range sentinel
		// dates outright - a far-future `end` fails every partition with
		// "invalid/unsupported date range" and the query returns zero hits
		// quickly, which reads as "no matches" rather than as an error.
		r := map[string]any{"field": "timestamp"}
		if f.FromISO != "" {
			r["start"] = f.FromISO
			r["inclusive_start"] = true
		}
		if f.ToISO != "" {
			r["end"] = f.ToISO
			r["inclusive_end"] = true
		}
		conj = append(conj, r)
	}
	if len(conj) == 1 {
		return conj[0]
	}
	return map[string]any{"conjuncts": conj}
}

// Query also returns the exact JSON body sent to FTS, so the search UI can
// expose the query behind a result set.
func (s *Search) Query(f SearchFilters, searchAfter string, size int) (SearchPage, string, error) {
	body := map[string]any{
		// FTS defaults to a 10s server-side timeout, which a deliberately broad
		// search over 500M documents exceeds.
		"ctl":   map[string]any{"timeout": 45000},
		"query": s.buildQuery(f),
		"size":  size,
		"from":  0,
		// Sort on the DOCUMENT ID, not a field. Ordering is identical because
		// every id is ntf::{tenant}::{ULID}::{channel} and queries are always
		// tenant-scoped. Measured at 139M hits: ~29% faster than sorting on the
		// sort_key field, because _id needs no field docvalues at all. Sorting
		// on the numeric `timestamp` is 2-4x SLOWER than either.
		"sort":   []string{"-_id"},
		"fields": []string{},
	}
	if searchAfter != "" {
		body["search_after"] = []string{searchAfter}
	}
	buf, _ := json.Marshal(body)
	pretty, _ := json.MarshalIndent(body, "", "  ")
	shown := string(pretty)
	req, err := http.NewRequest("POST",
		fmt.Sprintf("%s/api/index/%s/query", s.baseURL, s.index), bytes.NewReader(buf))
	if err != nil {
		return SearchPage{}, shown, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", s.auth)

	res, err := s.client.Do(req)
	if err != nil {
		return SearchPage{}, shown, err
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		return SearchPage{}, shown, fmt.Errorf("FTS query failed: HTTP %d", res.StatusCode)
	}
	var out struct {
		Hits []struct {
			ID   string   `json:"id"`
			Sort []string `json:"sort"`
		} `json:"hits"`
		TotalHits int64 `json:"total_hits"`
		Status    struct {
			Failed     int               `json:"failed"`
			Successful int               `json:"successful"`
			Errors     map[string]string `json:"errors"`
		} `json:"status"`
	}
	if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
		return SearchPage{}, shown, err
	}
	// Every partition failing is an ERROR, not an empty result set. Returning
	// zero hits here is indistinguishable from "nothing matched" - the failure
	// mode that once hid an invalid date range behind a fast, empty page.
	if out.Status.Failed > 0 && out.Status.Successful == 0 {
		for _, e := range out.Status.Errors {
			return SearchPage{}, shown, fmt.Errorf("FTS query failed on all %d partitions: %s",
				out.Status.Failed, e)
		}
		return SearchPage{}, shown, fmt.Errorf("FTS query failed on all %d partitions", out.Status.Failed)
	}
	page := SearchPage{
		TotalHits: out.TotalHits, FailedPartitions: out.Status.Failed,
		SuccessfulPartitions: out.Status.Successful,
	}
	for _, h := range out.Hits {
		page.IDs = append(page.IDs, h.ID)
	}
	if n := len(out.Hits); n > 0 {
		last := out.Hits[n-1]
		if len(last.Sort) > 0 {
			page.LastSort = last.Sort[0]
		} else {
			page.LastSort = last.ID
		}
	}
	return page, shown, nil
}

func (s *Search) IndexedCount() int64 {
	req, _ := http.NewRequest("GET", fmt.Sprintf("%s/api/index/%s/count", s.baseURL, s.index), nil)
	req.Header.Set("Authorization", s.auth)
	res, err := s.client.Do(req)
	if err != nil {
		return -1
	}
	defer res.Body.Close()
	var out struct {
		Count int64 `json:"count"`
	}
	_ = json.NewDecoder(res.Body).Decode(&out)
	return out.Count
}
