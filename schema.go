package otelhouse

import "fmt"

// ttlClause returns the ClickHouse TTL clause for the given timestamp column
// and retention. A negative value disables retention by returning an empty
// string so the surrounding CREATE TABLE statement omits TTL entirely.
func ttlClause(timeCol string, days int) string {
	if days < 0 {
		return ""
	}
	return fmt.Sprintf("TTL toDateTime(%s) + toIntervalDay(%d)", timeCol, days)
}

// schemaSQL returns the CREATE TABLE statement for the given traces table
// name. retentionDays controls the TTL clause: positive values set a TTL,
// negative values omit the clause entirely.
func schemaSQL(table string, retentionDays int) string {
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
%s
SETTINGS index_granularity = 8192, ttl_only_drop_parts = 1
`, table, ttlClause("Timestamp", retentionDays))
}

// metricsSchemaSQL returns the CREATE TABLE statements for the five metric
// tables (gauge, sum, histogram, exponential histogram, summary), keyed by
// table suffix. The names and columns mirror the OpenTelemetry Collector
// contrib ClickHouse exporter so Grafana dashboards built for that exporter
// work here too. retentionDays controls the TTL clause on every table:
// positive values set a TTL, negative values omit the clause entirely.
func metricsSchemaSQL(prefix string, retentionDays int) map[string]string {
	ttl := ttlClause("TimeUnix", retentionDays)

	gauge := fmt.Sprintf(`
CREATE TABLE IF NOT EXISTS %s_gauge
(
    ResourceAttributes    Map(LowCardinality(String), String)        CODEC(ZSTD(1)),
    ResourceSchemaUrl     String                                     CODEC(ZSTD(1)),
    ScopeName             String                                     CODEC(ZSTD(1)),
    ScopeVersion          String                                     CODEC(ZSTD(1)),
    ScopeAttributes       Map(LowCardinality(String), String)        CODEC(ZSTD(1)),
    ScopeDroppedAttrCount UInt32                                     CODEC(ZSTD(1)),
    ScopeSchemaUrl        String                                     CODEC(ZSTD(1)),
    ServiceName           LowCardinality(String)                     CODEC(ZSTD(1)),
    MetricName            String                                     CODEC(ZSTD(1)),
    MetricDescription     String                                     CODEC(ZSTD(1)),
    MetricUnit            String                                     CODEC(ZSTD(1)),
    Attributes            Map(LowCardinality(String), String)        CODEC(ZSTD(1)),
    StartTimeUnix         DateTime64(9)                              CODEC(Delta, ZSTD(1)),
    TimeUnix              DateTime64(9)                              CODEC(Delta, ZSTD(1)),
    Value                 Float64                                    CODEC(ZSTD(1)),
    Flags                 UInt32                                     CODEC(ZSTD(1))
)
ENGINE = MergeTree()
PARTITION BY toDate(TimeUnix)
ORDER BY (ServiceName, MetricName, toUnixTimestamp(TimeUnix))
%s
SETTINGS index_granularity = 8192, ttl_only_drop_parts = 1
`, prefix, ttl)

	sum := fmt.Sprintf(`
CREATE TABLE IF NOT EXISTS %s_sum
(
    ResourceAttributes     Map(LowCardinality(String), String)        CODEC(ZSTD(1)),
    ResourceSchemaUrl      String                                     CODEC(ZSTD(1)),
    ScopeName              String                                     CODEC(ZSTD(1)),
    ScopeVersion           String                                     CODEC(ZSTD(1)),
    ScopeAttributes        Map(LowCardinality(String), String)        CODEC(ZSTD(1)),
    ScopeDroppedAttrCount  UInt32                                     CODEC(ZSTD(1)),
    ScopeSchemaUrl         String                                     CODEC(ZSTD(1)),
    ServiceName            LowCardinality(String)                     CODEC(ZSTD(1)),
    MetricName             String                                     CODEC(ZSTD(1)),
    MetricDescription      String                                     CODEC(ZSTD(1)),
    MetricUnit             String                                     CODEC(ZSTD(1)),
    Attributes             Map(LowCardinality(String), String)        CODEC(ZSTD(1)),
    StartTimeUnix          DateTime64(9)                              CODEC(Delta, ZSTD(1)),
    TimeUnix               DateTime64(9)                              CODEC(Delta, ZSTD(1)),
    Value                  Float64                                    CODEC(ZSTD(1)),
    Flags                  UInt32                                     CODEC(ZSTD(1)),
    AggregationTemporality Int32                                      CODEC(ZSTD(1)),
    IsMonotonic            Bool                                       CODEC(ZSTD(1))
)
ENGINE = MergeTree()
PARTITION BY toDate(TimeUnix)
ORDER BY (ServiceName, MetricName, toUnixTimestamp(TimeUnix))
%s
SETTINGS index_granularity = 8192, ttl_only_drop_parts = 1
`, prefix, ttl)

	histogram := fmt.Sprintf(`
CREATE TABLE IF NOT EXISTS %s_histogram
(
    ResourceAttributes     Map(LowCardinality(String), String)        CODEC(ZSTD(1)),
    ResourceSchemaUrl      String                                     CODEC(ZSTD(1)),
    ScopeName              String                                     CODEC(ZSTD(1)),
    ScopeVersion           String                                     CODEC(ZSTD(1)),
    ScopeAttributes        Map(LowCardinality(String), String)        CODEC(ZSTD(1)),
    ScopeDroppedAttrCount  UInt32                                     CODEC(ZSTD(1)),
    ScopeSchemaUrl         String                                     CODEC(ZSTD(1)),
    ServiceName            LowCardinality(String)                     CODEC(ZSTD(1)),
    MetricName             String                                     CODEC(ZSTD(1)),
    MetricDescription      String                                     CODEC(ZSTD(1)),
    MetricUnit             String                                     CODEC(ZSTD(1)),
    Attributes             Map(LowCardinality(String), String)        CODEC(ZSTD(1)),
    StartTimeUnix          DateTime64(9)                              CODEC(Delta, ZSTD(1)),
    TimeUnix               DateTime64(9)                              CODEC(Delta, ZSTD(1)),
    Count                  UInt64                                     CODEC(ZSTD(1)),
    Sum                    Float64                                    CODEC(ZSTD(1)),
    BucketCounts           Array(UInt64)                              CODEC(ZSTD(1)),
    ExplicitBounds         Array(Float64)                             CODEC(ZSTD(1)),
    Min                    Float64                                    CODEC(ZSTD(1)),
    Max                    Float64                                    CODEC(ZSTD(1)),
    Flags                  UInt32                                     CODEC(ZSTD(1)),
    AggregationTemporality Int32                                      CODEC(ZSTD(1))
)
ENGINE = MergeTree()
PARTITION BY toDate(TimeUnix)
ORDER BY (ServiceName, MetricName, toUnixTimestamp(TimeUnix))
%s
SETTINGS index_granularity = 8192, ttl_only_drop_parts = 1
`, prefix, ttl)

	expHistogram := fmt.Sprintf(`
CREATE TABLE IF NOT EXISTS %s_exponential_histogram
(
    ResourceAttributes     Map(LowCardinality(String), String)        CODEC(ZSTD(1)),
    ResourceSchemaUrl      String                                     CODEC(ZSTD(1)),
    ScopeName              String                                     CODEC(ZSTD(1)),
    ScopeVersion           String                                     CODEC(ZSTD(1)),
    ScopeAttributes        Map(LowCardinality(String), String)        CODEC(ZSTD(1)),
    ScopeDroppedAttrCount  UInt32                                     CODEC(ZSTD(1)),
    ScopeSchemaUrl         String                                     CODEC(ZSTD(1)),
    ServiceName            LowCardinality(String)                     CODEC(ZSTD(1)),
    MetricName             String                                     CODEC(ZSTD(1)),
    MetricDescription      String                                     CODEC(ZSTD(1)),
    MetricUnit             String                                     CODEC(ZSTD(1)),
    Attributes             Map(LowCardinality(String), String)        CODEC(ZSTD(1)),
    StartTimeUnix          DateTime64(9)                              CODEC(Delta, ZSTD(1)),
    TimeUnix               DateTime64(9)                              CODEC(Delta, ZSTD(1)),
    Count                  UInt64                                     CODEC(ZSTD(1)),
    Sum                    Float64                                    CODEC(ZSTD(1)),
    Scale                  Int32                                      CODEC(ZSTD(1)),
    ZeroCount              UInt64                                     CODEC(ZSTD(1)),
    PositiveOffset         Int32                                      CODEC(ZSTD(1)),
    PositiveBucketCounts   Array(UInt64)                              CODEC(ZSTD(1)),
    NegativeOffset         Int32                                      CODEC(ZSTD(1)),
    NegativeBucketCounts   Array(UInt64)                              CODEC(ZSTD(1)),
    ZeroThreshold          Float64                                    CODEC(ZSTD(1)),
    Min                    Float64                                    CODEC(ZSTD(1)),
    Max                    Float64                                    CODEC(ZSTD(1)),
    Flags                  UInt32                                     CODEC(ZSTD(1)),
    AggregationTemporality Int32                                      CODEC(ZSTD(1))
)
ENGINE = MergeTree()
PARTITION BY toDate(TimeUnix)
ORDER BY (ServiceName, MetricName, toUnixTimestamp(TimeUnix))
%s
SETTINGS index_granularity = 8192, ttl_only_drop_parts = 1
`, prefix, ttl)

	summary := fmt.Sprintf(`
CREATE TABLE IF NOT EXISTS %s_summary
(
    ResourceAttributes    Map(LowCardinality(String), String)        CODEC(ZSTD(1)),
    ResourceSchemaUrl     String                                     CODEC(ZSTD(1)),
    ScopeName             String                                     CODEC(ZSTD(1)),
    ScopeVersion          String                                     CODEC(ZSTD(1)),
    ScopeAttributes       Map(LowCardinality(String), String)        CODEC(ZSTD(1)),
    ScopeDroppedAttrCount UInt32                                     CODEC(ZSTD(1)),
    ScopeSchemaUrl        String                                     CODEC(ZSTD(1)),
    ServiceName           LowCardinality(String)                     CODEC(ZSTD(1)),
    MetricName            String                                     CODEC(ZSTD(1)),
    MetricDescription     String                                     CODEC(ZSTD(1)),
    MetricUnit            String                                     CODEC(ZSTD(1)),
    Attributes            Map(LowCardinality(String), String)        CODEC(ZSTD(1)),
    StartTimeUnix         DateTime64(9)                              CODEC(Delta, ZSTD(1)),
    TimeUnix              DateTime64(9)                              CODEC(Delta, ZSTD(1)),
    Count                 UInt64                                     CODEC(ZSTD(1)),
    Sum                   Float64                                    CODEC(ZSTD(1)),
    Quantiles             Array(Float64)                             CODEC(ZSTD(1)),
    Values                Array(Float64)                             CODEC(ZSTD(1)),
    Flags                 UInt32                                     CODEC(ZSTD(1))
)
ENGINE = MergeTree()
PARTITION BY toDate(TimeUnix)
ORDER BY (ServiceName, MetricName, toUnixTimestamp(TimeUnix))
%s
SETTINGS index_granularity = 8192, ttl_only_drop_parts = 1
`, prefix, ttl)

	return map[string]string{
		"gauge":                 gauge,
		"sum":                   sum,
		"histogram":             histogram,
		"exponential_histogram": expHistogram,
		"summary":               summary,
	}
}
