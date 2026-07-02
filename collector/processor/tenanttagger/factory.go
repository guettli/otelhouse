package tenanttagger

import (
	"context"

	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/consumer"
	"go.opentelemetry.io/collector/pdata/plog"
	"go.opentelemetry.io/collector/pdata/pmetric"
	"go.opentelemetry.io/collector/pdata/ptrace"
	"go.opentelemetry.io/collector/processor"
	"go.opentelemetry.io/collector/processor/processorhelper"
)

var typeStr = component.MustNewType("tenanttagger")

// NewFactory returns a processor.Factory for use in the ocb builder-config.
func NewFactory() processor.Factory {
	return processor.NewFactory(
		typeStr,
		createDefaultConfig,
		processor.WithTraces(createTracesProcessor, component.StabilityLevelAlpha),
		processor.WithLogs(createLogsProcessor, component.StabilityLevelAlpha),
		processor.WithMetrics(createMetricsProcessor, component.StabilityLevelAlpha),
	)
}

func createDefaultConfig() component.Config {
	return &Config{
		AttributeKey:  defaultAttributeKey,
		AuthAttribute: defaultAuthAttribute,
	}
}

func createTracesProcessor(
	ctx context.Context,
	set processor.Settings,
	cfg component.Config,
	nextConsumer consumer.Traces,
) (processor.Traces, error) {
	pCfg := cfg.(*Config)
	return processorhelper.NewTraces(
		ctx, set, cfg, nextConsumer,
		func(ctx context.Context, td ptrace.Traces) (ptrace.Traces, error) {
			return processTraces(ctx, *pCfg, td)
		},
		processorhelper.WithCapabilities(consumer.Capabilities{MutatesData: true}),
	)
}

func createLogsProcessor(
	ctx context.Context,
	set processor.Settings,
	cfg component.Config,
	nextConsumer consumer.Logs,
) (processor.Logs, error) {
	pCfg := cfg.(*Config)
	return processorhelper.NewLogs(
		ctx, set, cfg, nextConsumer,
		func(ctx context.Context, ld plog.Logs) (plog.Logs, error) {
			return processLogs(ctx, *pCfg, ld)
		},
		processorhelper.WithCapabilities(consumer.Capabilities{MutatesData: true}),
	)
}

func createMetricsProcessor(
	ctx context.Context,
	set processor.Settings,
	cfg component.Config,
	nextConsumer consumer.Metrics,
) (processor.Metrics, error) {
	pCfg := cfg.(*Config)
	return processorhelper.NewMetrics(
		ctx, set, cfg, nextConsumer,
		func(ctx context.Context, md pmetric.Metrics) (pmetric.Metrics, error) {
			return processMetrics(ctx, *pCfg, md)
		},
		processorhelper.WithCapabilities(consumer.Capabilities{MutatesData: true}),
	)
}
