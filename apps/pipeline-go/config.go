package main

type Config struct {
	Port                   int
	CBHost, CBUser, CBPass string
	Bucket, Scope          string
	TenantID               string
	MockChannelsURL        string
	FTSURL, FTSIndex       string
	PolicyRefreshMs        int
	RetryTickMs            int
	RetryBatch             int
	RetryHorizonMs         int
	DeliveryWorkers        int
	StaleAfterMs           int
	DefaultTextWindowMs    int
	KvPoolSize             int
}

func LoadConfig() Config {
	host := env("CB_HOST", "127.0.0.1")
	return Config{
		Port:            envInt("PORT", 8080),
		CBHost:          host,
		CBUser:          env("CB_USER", "Administrator"),
		CBPass:          env("CB_PASS", "password"),
		Bucket:          env("CB_BUCKET", "ncgr"),
		Scope:           env("CB_SCOPE", "platform"),
		TenantID:        env("TENANT_ID", "ncgr"),
		MockChannelsURL: env("MOCK_CHANNELS_URL", "http://mock-channels:8081"),
		FTSURL:          env("FTS_URL", "http://"+host+":8091/_p/fts"),
		FTSIndex:        env("FTS_INDEX", "fts_notifications"),
		PolicyRefreshMs: envInt("POLICY_REFRESH_MS", 10_000),
		RetryTickMs:     envInt("RETRY_TICK_MS", 1000),
		// Drain rate is exactly RetryBatch per tick. At 500/sec, recovering the
		// backlog left by a restart during a 10,000/sec run took ~12 minutes.
		// The scheduler's own query is served by the small partial index
		// idx_retry, so a larger batch costs little.
		RetryBatch:     envInt("RETRY_BATCH", 5000),
		RetryHorizonMs: envInt("RETRY_HORIZON_MS", 3_600_000),
		// One process, so this is real parallelism across cores rather than
		// concurrency on a single thread.
		DeliveryWorkers:     envInt("DELIVERY_WORKERS", 256),
		StaleAfterMs:        envInt("STALE_AFTER_MS", 60_000),
		DefaultTextWindowMs: envInt("DEFAULT_TEXT_WINDOW_MS", 7*86_400_000),
		KvPoolSize:          envInt("KV_POOL_SIZE", 8),
	}
}
