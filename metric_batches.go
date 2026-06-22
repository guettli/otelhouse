package otelhouse

import (
	"context"
	"fmt"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"go.opentelemetry.io/otel/sdk/instrumentation"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

// metricBatches groups one PrepareBatch per metric sub-table so a single
// Export call commits with at most five round trips.
type metricBatches struct {
	gauge    driver.Batch
	sum      driver.Batch
	histo    driver.Batch
	expHisto driver.Batch
	summary  driver.Batch
}

func newMetricBatches(ctx context.Context, conn driver.Conn, prefix string) (*metricBatches, error) {
	b := &metricBatches{}
	var err error
	if b.gauge, err = conn.PrepareBatch(ctx, "INSERT INTO "+prefix+"_gauge"); err != nil {
		return nil, fmt.Errorf("prepare gauge batch: %w", err)
	}
	if b.sum, err = conn.PrepareBatch(ctx, "INSERT INTO "+prefix+"_sum"); err != nil {
		return nil, fmt.Errorf("prepare sum batch: %w", err)
	}
	if b.histo, err = conn.PrepareBatch(ctx, "INSERT INTO "+prefix+"_histogram"); err != nil {
		return nil, fmt.Errorf("prepare histogram batch: %w", err)
	}
	if b.expHisto, err = conn.PrepareBatch(ctx, "INSERT INTO "+prefix+"_exponential_histogram"); err != nil {
		return nil, fmt.Errorf("prepare exponential_histogram batch: %w", err)
	}
	if b.summary, err = conn.PrepareBatch(ctx, "INSERT INTO "+prefix+"_summary"); err != nil {
		return nil, fmt.Errorf("prepare summary batch: %w", err)
	}
	return b, nil
}

func (b *metricBatches) appendMetric(
	svcName string,
	resAttrs map[string]string,
	resSchema string,
	scope instrumentation.Scope,
	scopeAttrs map[string]string,
	m metricdata.Metrics,
) error {
	switch d := m.Data.(type) {
	case metricdata.Gauge[int64]:
		for _, dp := range d.DataPoints {
			if err := b.gauge.Append(
				resAttrs, resSchema,
				scope.Name, scope.Version, scopeAttrs, uint32(0), scope.SchemaURL,
				svcName, m.Name, m.Description, m.Unit,
				kvToMap(dp.Attributes.ToSlice()),
				dp.StartTime, dp.Time,
				float64(dp.Value),
				uint32(0),
			); err != nil {
				return fmt.Errorf("append gauge %s: %w", m.Name, err)
			}
		}
	case metricdata.Gauge[float64]:
		for _, dp := range d.DataPoints {
			if err := b.gauge.Append(
				resAttrs, resSchema,
				scope.Name, scope.Version, scopeAttrs, uint32(0), scope.SchemaURL,
				svcName, m.Name, m.Description, m.Unit,
				kvToMap(dp.Attributes.ToSlice()),
				dp.StartTime, dp.Time,
				dp.Value,
				uint32(0),
			); err != nil {
				return fmt.Errorf("append gauge %s: %w", m.Name, err)
			}
		}
	case metricdata.Sum[int64]:
		for _, dp := range d.DataPoints {
			if err := b.sum.Append(
				resAttrs, resSchema,
				scope.Name, scope.Version, scopeAttrs, uint32(0), scope.SchemaURL,
				svcName, m.Name, m.Description, m.Unit,
				kvToMap(dp.Attributes.ToSlice()),
				dp.StartTime, dp.Time,
				float64(dp.Value),
				uint32(0),
				int32(d.Temporality), d.IsMonotonic,
			); err != nil {
				return fmt.Errorf("append sum %s: %w", m.Name, err)
			}
		}
	case metricdata.Sum[float64]:
		for _, dp := range d.DataPoints {
			if err := b.sum.Append(
				resAttrs, resSchema,
				scope.Name, scope.Version, scopeAttrs, uint32(0), scope.SchemaURL,
				svcName, m.Name, m.Description, m.Unit,
				kvToMap(dp.Attributes.ToSlice()),
				dp.StartTime, dp.Time,
				dp.Value,
				uint32(0),
				int32(d.Temporality), d.IsMonotonic,
			); err != nil {
				return fmt.Errorf("append sum %s: %w", m.Name, err)
			}
		}
	case metricdata.Histogram[int64]:
		for _, dp := range d.DataPoints {
			minV, _ := dp.Min.Value()
			maxV, _ := dp.Max.Value()
			if err := b.histo.Append(
				resAttrs, resSchema,
				scope.Name, scope.Version, scopeAttrs, uint32(0), scope.SchemaURL,
				svcName, m.Name, m.Description, m.Unit,
				kvToMap(dp.Attributes.ToSlice()),
				dp.StartTime, dp.Time,
				dp.Count, float64(dp.Sum),
				dp.BucketCounts, dp.Bounds,
				float64(minV), float64(maxV),
				uint32(0),
				int32(d.Temporality),
			); err != nil {
				return fmt.Errorf("append histogram %s: %w", m.Name, err)
			}
		}
	case metricdata.Histogram[float64]:
		for _, dp := range d.DataPoints {
			minV, _ := dp.Min.Value()
			maxV, _ := dp.Max.Value()
			if err := b.histo.Append(
				resAttrs, resSchema,
				scope.Name, scope.Version, scopeAttrs, uint32(0), scope.SchemaURL,
				svcName, m.Name, m.Description, m.Unit,
				kvToMap(dp.Attributes.ToSlice()),
				dp.StartTime, dp.Time,
				dp.Count, dp.Sum,
				dp.BucketCounts, dp.Bounds,
				minV, maxV,
				uint32(0),
				int32(d.Temporality),
			); err != nil {
				return fmt.Errorf("append histogram %s: %w", m.Name, err)
			}
		}
	case metricdata.ExponentialHistogram[int64]:
		for _, dp := range d.DataPoints {
			minV, _ := dp.Min.Value()
			maxV, _ := dp.Max.Value()
			if err := b.expHisto.Append(
				resAttrs, resSchema,
				scope.Name, scope.Version, scopeAttrs, uint32(0), scope.SchemaURL,
				svcName, m.Name, m.Description, m.Unit,
				kvToMap(dp.Attributes.ToSlice()),
				dp.StartTime, dp.Time,
				dp.Count, float64(dp.Sum),
				dp.Scale, dp.ZeroCount,
				dp.PositiveBucket.Offset, dp.PositiveBucket.Counts,
				dp.NegativeBucket.Offset, dp.NegativeBucket.Counts,
				dp.ZeroThreshold,
				float64(minV), float64(maxV),
				uint32(0),
				int32(d.Temporality),
			); err != nil {
				return fmt.Errorf("append exponential_histogram %s: %w", m.Name, err)
			}
		}
	case metricdata.ExponentialHistogram[float64]:
		for _, dp := range d.DataPoints {
			minV, _ := dp.Min.Value()
			maxV, _ := dp.Max.Value()
			if err := b.expHisto.Append(
				resAttrs, resSchema,
				scope.Name, scope.Version, scopeAttrs, uint32(0), scope.SchemaURL,
				svcName, m.Name, m.Description, m.Unit,
				kvToMap(dp.Attributes.ToSlice()),
				dp.StartTime, dp.Time,
				dp.Count, dp.Sum,
				dp.Scale, dp.ZeroCount,
				dp.PositiveBucket.Offset, dp.PositiveBucket.Counts,
				dp.NegativeBucket.Offset, dp.NegativeBucket.Counts,
				dp.ZeroThreshold,
				minV, maxV,
				uint32(0),
				int32(d.Temporality),
			); err != nil {
				return fmt.Errorf("append exponential_histogram %s: %w", m.Name, err)
			}
		}
	case metricdata.Summary:
		for _, dp := range d.DataPoints {
			quantiles := make([]float64, len(dp.QuantileValues))
			values := make([]float64, len(dp.QuantileValues))
			for i, qv := range dp.QuantileValues {
				quantiles[i] = qv.Quantile
				values[i] = qv.Value
			}
			if err := b.summary.Append(
				resAttrs, resSchema,
				scope.Name, scope.Version, scopeAttrs, uint32(0), scope.SchemaURL,
				svcName, m.Name, m.Description, m.Unit,
				kvToMap(dp.Attributes.ToSlice()),
				dp.StartTime, dp.Time,
				dp.Count, dp.Sum,
				quantiles, values,
				uint32(0),
			); err != nil {
				return fmt.Errorf("append summary %s: %w", m.Name, err)
			}
		}
	default:
		return fmt.Errorf("unsupported aggregation type for metric %s: %T", m.Name, m.Data)
	}
	return nil
}

func (b *metricBatches) send() error {
	if err := sendOrAbort(b.gauge); err != nil {
		return fmt.Errorf("send gauge batch: %w", err)
	}
	if err := sendOrAbort(b.sum); err != nil {
		return fmt.Errorf("send sum batch: %w", err)
	}
	if err := sendOrAbort(b.histo); err != nil {
		return fmt.Errorf("send histogram batch: %w", err)
	}
	if err := sendOrAbort(b.expHisto); err != nil {
		return fmt.Errorf("send exponential_histogram batch: %w", err)
	}
	if err := sendOrAbort(b.summary); err != nil {
		return fmt.Errorf("send summary batch: %w", err)
	}
	return nil
}

// sendOrAbort sends the batch when it has rows, or aborts the empty prepared
// batch so its server-side resources are released. An empty INSERT block can
// fail to serialize for the native protocol, so emptiness is checked up-front.
func sendOrAbort(batch driver.Batch) error {
	if batch.Rows() == 0 {
		return batch.Abort()
	}
	return batch.Send()
}

// closeAll releases the prepared-batch resources. Safe to call after send():
// Close is a no-op once IsSent is true.
func (b *metricBatches) closeAll() {
	_ = b.gauge.Close()
	_ = b.sum.Close()
	_ = b.histo.Close()
	_ = b.expHisto.Close()
	_ = b.summary.Close()
}
