package otelhouse

import "fmt"

// schemaSQL returns the CREATE TABLE statement for the given table name.
func schemaSQL(table string) string {
	return fmt.Sprintf(`
CREATE TABLE IF NOT EXISTS %s
(
    Timestamp          DateTime64(9)                              CODEC(Delta, ZSTD(1)),
    TraceId            String                                     CODEC(ZSTD(1)),
    SpanId             String                                     CODEC(ZSTD(1)),
    ParentSpanId       String                                     CODEC(ZSTD(1)),
    TraceState         String                                     CODEC(ZSTD(1)),
    SpanName           LowCardinality(String)                     CODEC(ZSTD(1)),
    SpanKind           LowCardinality(String)                     CODEC(ZSTD(1)),
    ServiceName        LowCardinality(String)                     CODEC(ZSTD(1)),
    ResourceAttributes Map(LowCardinality(String), String)        CODEC(ZSTD(1)),
    ScopeName          String                                     CODEC(ZSTD(1)),
    ScopeVersion       String                                     CODEC(ZSTD(1)),
    SpanAttributes     Map(LowCardinality(String), String)        CODEC(ZSTD(1)),
    Duration           Int64                                      CODEC(ZSTD(1)),
    StatusCode         LowCardinality(String)                     CODEC(ZSTD(1)),
    StatusMessage      String                                     CODEC(ZSTD(1)),
    EventTimestamps    Array(DateTime64(9))                       CODEC(ZSTD(1)),
    EventNames         Array(LowCardinality(String))              CODEC(ZSTD(1)),
    EventAttributes    Array(Map(LowCardinality(String), String)) CODEC(ZSTD(1)),
    LinkTraceIds       Array(String)                              CODEC(ZSTD(1)),
    LinkSpanIds        Array(String)                              CODEC(ZSTD(1)),
    LinkTraceStates    Array(String)                              CODEC(ZSTD(1)),
    LinkAttributes     Array(Map(LowCardinality(String), String)) CODEC(ZSTD(1))
)
ENGINE = MergeTree()
PARTITION BY toDate(Timestamp)
ORDER BY (ServiceName, SpanName, toUnixTimestamp(Timestamp))
TTL toDateTime(Timestamp) + toIntervalDay(180)
SETTINGS index_granularity = 8192, ttl_only_drop_parts = 1
`, table)
}
