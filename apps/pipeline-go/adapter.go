package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// Channel Adapter: consumes delivery commands, calls the provider, publishes
// the result. Isolates the platform from provider protocols (the requirements'
// "Channel
// Adapters"); here the provider is the mock-channels service.

type Adapter struct {
	baseURL string
	bus     *Bus
	metrics *Metrics
	trace   *TraceStore
	client  *http.Client
	workers int
}

func NewAdapter(baseURL string, bus *Bus, m *Metrics, tr *TraceStore, workers int) *Adapter {
	return &Adapter{
		baseURL: baseURL, bus: bus, metrics: m, trace: tr, workers: workers,
		client: &http.Client{
			Timeout: 15 * time.Second,
			Transport: &http.Transport{
				// Without a large keep-alive pool, thousands of short-lived
				// connections per second dominate CPU and we measure ourselves
				// rather than the pipeline.
				MaxIdleConns:        workers * 2,
				MaxIdleConnsPerHost: workers * 2,
				MaxConnsPerHost:     workers * 4,
				IdleConnTimeout:     60 * time.Second,
			},
		},
	}
}

func (a *Adapter) Start() {
	for _, ch := range AllChannels {
		a.bus.Subscribe(topicDelivery(ch), a.workers, func(msg any) {
			cmd, ok := msg.(DeliveryCommand)
			if !ok {
				return
			}
			a.deliver(cmd)
		})
	}
}

func (a *Adapter) deliver(cmd DeliveryCommand) {
	t0 := time.Now()
	body, _ := json.Marshal(map[string]string{
		"notification_id": cmd.NotificationID,
		"user_id":         cmd.UserID,
		"subject":         cmd.Subject,
		"message":         cmd.Message,
	})
	req, err := http.NewRequest("POST", a.baseURL+"/"+cmd.Channel+"/send", bytes.NewReader(body))
	var status int
	var errMsg string
	okFlag := false

	if err == nil {
		req.Header.Set("Content-Type", "application/json")
		if cmd.ForceFail {
			req.Header.Set("x-force-fail", "true")
		}
		if cmd.ForcePermanent {
			req.Header.Set("x-force-permanent", "true")
		}
		ct := time.Now()
		res, rerr := a.client.Do(req)
		a.metrics.Record("channel_call", time.Since(ct).Microseconds())
		if rerr != nil {
			// Transport failure or timeout: no HTTP status, classified retryable.
			errMsg = rerr.Error()
		} else {
			status = res.StatusCode
			okFlag = status >= 200 && status < 300
			var payload struct {
				Error string `json:"error"`
			}
			_ = json.NewDecoder(res.Body).Decode(&payload)
			res.Body.Close()
			if !okFlag {
				errMsg = payload.Error
				if errMsg == "" {
					errMsg = fmt.Sprintf("HTTP %d", status)
				}
			}
		}
	} else {
		errMsg = err.Error()
	}

	latency := time.Since(t0).Milliseconds()
	verdict := "FAILED"
	if okFlag {
		verdict = "ok"
	}
	a.trace.Add(cmd.EventID, "attempt", cmd.Channel,
		fmt.Sprintf("trial %d %s %d (%dms)", cmd.Trials+1, verdict, status, latency))

	a.bus.Publish(topicResults(cmd.Channel), DeliveryResult{
		Cmd: cmd, OK: okFlag, HTTPStatus: status, Err: errMsg, LatencyMs: latency,
	})
}

func checkChannels(baseURL string) (bool, string) {
	c := &http.Client{Timeout: 5 * time.Second}
	res, err := c.Get(baseURL + "/health")
	if err != nil {
		return false, err.Error()
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		return false, fmt.Sprintf("HTTP %d", res.StatusCode)
	}
	return true, "reachable"
}
